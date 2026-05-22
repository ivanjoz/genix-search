// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

mod spanish_bigrams;

use self::spanish_bigrams::LETTER_PAIR_ROWS;

pub const NUM_FLAG: u8 = 255;
pub const INDEX_COMPACT_PAIR: u8 = 1;
pub const INDEX_EXTENDED_PAIR: u8 = 2;
pub const INDEX_DOC_REVERSE: u8 = 4;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WordSignature {
    Short(u32),
    Long(u64),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PairKind {
    Adjacent = 1,
    Wrap = 2,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WordSlot {
    Short(u32),
    Long(u64),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PairSignatureRef {
    pub index: u8,
    pub pair_kind: PairKind,
    pub word1: WordSlot,
    pub word2: WordSlot,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PairEntry {
    pub pair_kind: PairKind,
    pub word1: WordSlot,
    pub word2: WordSlot,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EncodedWord {
    pub token: String,
    pub units: Vec<u8>,
    pub signature: WordSignature,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EncodedPrefix {
    pub token: String,
    pub unit_prefixes: Vec<Vec<u8>>,
}

pub fn normalize(input: &str) -> Vec<String> {
    let mut tokens = Vec::new();
    let mut current = String::new();
    let mut last_letter = None;

    for source_char in input.chars() {
        match normalize_char(source_char) {
            Some(normalized) if normalized.is_ascii_alphabetic() => {
                if last_letter != Some(normalized) {
                    current.push(normalized);
                }

                last_letter = Some(normalized);
            }
            Some(normalized) if normalized.is_ascii_digit() => {
                current.push(normalized);
                last_letter = None;
            }
            _ => {
                if !current.is_empty() {
                    tokens.push(current);
                    current = String::new();
                }

                last_letter = None;
            }
        }
    }

    if !current.is_empty() {
        tokens.push(current);
    }

    tokens
}

pub fn encode_text(input: &str) -> Vec<EncodedWord> {
    normalize(input)
        .into_iter()
        .filter_map(|token| encode_normalized_token(&token))
        .collect()
}

pub fn encode_prefix_text(input: &str) -> (Vec<EncodedWord>, Option<EncodedPrefix>) {
    let mut complete_terms = Vec::new();
    let mut prefix = None;

    for raw_term in input.split_whitespace() {
        if let Some(prefix_term) = raw_term.strip_suffix('*') {
            if prefix.is_none() {
                prefix = encode_prefix_token(prefix_term);
            }
        } else {
            complete_terms.extend(
                normalize(raw_term)
                    .into_iter()
                    .filter_map(|token| encode_normalized_token(&token)),
            );
        }
    }

    (complete_terms, prefix)
}

pub fn encode_prefix_token(input: &str) -> Option<EncodedPrefix> {
    let token = normalize(input).into_iter().next()?;
    let bytes = token.as_bytes();
    let mut units = Vec::with_capacity(8);
    let mut index = 0;
    let mut trailing_letter = None;

    while index < bytes.len() && units.len() < 8 {
        let current = bytes[index];

        if current.is_ascii_digit() {
            if units.len() + 2 > 8 {
                break;
            }

            let number_start = index;

            while index < bytes.len() && bytes[index].is_ascii_digit() {
                index += 1;
            }

            units.push(NUM_FLAG);
            units.push(bucket_number(&token[number_start..index]));
        } else if current.is_ascii_lowercase() {
            if index + 1 < bytes.len() && bytes[index + 1].is_ascii_lowercase() {
                units.push(letter_pair_code(current as char, bytes[index + 1] as char));
                index += 2;
            } else {
                trailing_letter = Some(current as char);
                break;
            }
        } else {
            index += 1;
        }
    }

    let unit_prefixes = if let Some(letter) = trailing_letter {
        if units.len() >= 8 {
            vec![units]
        } else {
            row_pair_codes(letter)
                .into_iter()
                .map(|code| {
                    let mut expanded_units = units.clone();

                    expanded_units.push(code);
                    expanded_units
                })
                .collect()
        }
    } else {
        vec![units]
    };

    if unit_prefixes.iter().all(Vec::is_empty) {
        None
    } else {
        Some(EncodedPrefix {
            token,
            unit_prefixes,
        })
    }
}

pub fn pair_entries(words: &[EncodedWord]) -> Vec<PairEntry> {
    let mut entries = Vec::new();

    if words.is_empty() {
        return entries;
    }

    for pair in words.windows(2) {
        entries.push(PairEntry {
            pair_kind: PairKind::Adjacent,
            word1: pair[0].slot(),
            word2: pair[1].slot(),
        });
    }

    entries.push(PairEntry {
        pair_kind: PairKind::Wrap,
        word1: words[words.len() - 1].slot(),
        word2: words[0].slot(),
    });

    entries
}

pub fn encode_normalized_token(token: &str) -> Option<EncodedWord> {
    if token.is_empty() {
        return None;
    }

    let bytes = token.as_bytes();
    let mut units = Vec::with_capacity(8);
    let mut index = 0;

    while index < bytes.len() && units.len() < 8 {
        let current = bytes[index];

        if current.is_ascii_digit() {
            if units.len() + 2 > 8 {
                break;
            }

            let number_start = index;

            while index < bytes.len() && bytes[index].is_ascii_digit() {
                index += 1;
            }

            units.push(NUM_FLAG);
            units.push(bucket_number(&token[number_start..index]));
        } else if current.is_ascii_lowercase() {
            if index + 1 < bytes.len() && bytes[index + 1].is_ascii_lowercase() {
                units.push(letter_pair_code(current as char, bytes[index + 1] as char));
                index += 2;
            } else {
                units.push(letter_pair_code(current as char, '$'));
                index += 1;
            }
        } else {
            index += 1;
        }
    }

    let signature = if units.len() <= 4 {
        WordSignature::Short(pack_u32(&units))
    } else {
        WordSignature::Long(pack_u64(&units))
    };

    Some(EncodedWord {
        token: token.to_string(),
        units,
        signature,
    })
}

pub fn compact_pair_key(
    collection_hash: u32,
    bucket_hash: u32,
    word1_sig32: u32,
    word2_sig32: u32,
    pair_kind: PairKind,
) -> Vec<u8> {
    let mut key = Vec::with_capacity(18);

    key.push(INDEX_COMPACT_PAIR);
    key.extend_from_slice(&collection_hash.to_be_bytes());
    key.extend_from_slice(&bucket_hash.to_be_bytes());
    key.extend_from_slice(&word1_sig32.to_be_bytes());
    key.extend_from_slice(&word2_sig32.to_be_bytes());
    key.push(pair_kind as u8);

    key
}

pub fn compact_pair_bucket_prefix(collection_hash: u32, bucket_hash: u32) -> Vec<u8> {
    pair_bucket_prefix(INDEX_COMPACT_PAIR, collection_hash, bucket_hash)
}

pub fn compact_pair_first_word_prefix(
    collection_hash: u32,
    bucket_hash: u32,
    word1_sig32: u32,
) -> Vec<u8> {
    let mut key = compact_pair_bucket_prefix(collection_hash, bucket_hash);

    key.extend_from_slice(&word1_sig32.to_be_bytes());

    key
}

pub fn compact_pair_first_word_unit_prefix(
    collection_hash: u32,
    bucket_hash: u32,
    word1_units: &[u8],
) -> Vec<u8> {
    let mut key = compact_pair_bucket_prefix(collection_hash, bucket_hash);

    key.extend_from_slice(&word1_units[..word1_units.len().min(4)]);

    key
}

pub fn compact_pair_second_word_unit_prefix(
    collection_hash: u32,
    bucket_hash: u32,
    word1_sig32: u32,
    word2_units: &[u8],
) -> Vec<u8> {
    let mut key = compact_pair_first_word_prefix(collection_hash, bucket_hash, word1_sig32);

    key.extend_from_slice(&word2_units[..word2_units.len().min(4)]);

    key
}

pub fn extended_pair_key(
    collection_hash: u32,
    bucket_hash: u32,
    word1: WordSlot,
    word2: WordSlot,
    pair_kind: PairKind,
) -> Vec<u8> {
    let mut key = Vec::with_capacity(28);

    key.push(INDEX_EXTENDED_PAIR);
    key.extend_from_slice(&collection_hash.to_be_bytes());
    key.extend_from_slice(&bucket_hash.to_be_bytes());
    encode_word_slot(word1, &mut key);
    encode_word_slot(word2, &mut key);
    key.push(pair_kind as u8);

    key
}

pub fn extended_pair_bucket_prefix(collection_hash: u32, bucket_hash: u32) -> Vec<u8> {
    pair_bucket_prefix(INDEX_EXTENDED_PAIR, collection_hash, bucket_hash)
}

pub fn extended_pair_first_word_prefix(
    collection_hash: u32,
    bucket_hash: u32,
    word1: WordSlot,
) -> Vec<u8> {
    let mut key = extended_pair_bucket_prefix(collection_hash, bucket_hash);

    encode_word_slot(word1, &mut key);

    key
}

pub fn extended_pair_first_word_unit_prefix(
    collection_hash: u32,
    bucket_hash: u32,
    word1_kind: u8,
    word1_units: &[u8],
) -> Vec<u8> {
    let mut key = extended_pair_bucket_prefix(collection_hash, bucket_hash);

    key.push(word1_kind);
    key.extend_from_slice(&word1_units[..word1_units.len().min(8)]);

    key
}

pub fn extended_pair_second_word_unit_prefix(
    collection_hash: u32,
    bucket_hash: u32,
    word1: WordSlot,
    word2_kind: u8,
    word2_units: &[u8],
) -> Vec<u8> {
    let mut key = extended_pair_bucket_prefix(collection_hash, bucket_hash);

    encode_word_slot(word1, &mut key);
    key.push(word2_kind);
    key.extend_from_slice(&word2_units[..word2_units.len().min(8)]);

    key
}

pub fn pair_key(collection_hash: u32, bucket_hash: u32, entry: PairEntry) -> Vec<u8> {
    match (entry.word1, entry.word2) {
        (WordSlot::Short(word1), WordSlot::Short(word2)) => {
            compact_pair_key(collection_hash, bucket_hash, word1, word2, entry.pair_kind)
        }
        _ => extended_pair_key(
            collection_hash,
            bucket_hash,
            entry.word1,
            entry.word2,
            entry.pair_kind,
        ),
    }
}

pub fn pair_signature_ref(entry: PairEntry) -> PairSignatureRef {
    PairSignatureRef {
        index: match (entry.word1, entry.word2) {
            (WordSlot::Short(_), WordSlot::Short(_)) => INDEX_COMPACT_PAIR,
            _ => INDEX_EXTENDED_PAIR,
        },
        pair_kind: entry.pair_kind,
        word1: entry.word1,
        word2: entry.word2,
    }
}

pub fn doc_reverse_key(collection_hash: u32, bucket_hash: u32, doc_id: i32) -> Vec<u8> {
    let mut key = Vec::with_capacity(13);

    key.push(INDEX_DOC_REVERSE);
    key.extend_from_slice(&collection_hash.to_be_bytes());
    key.extend_from_slice(&bucket_hash.to_be_bytes());
    key.extend_from_slice(&sortable_doc_id(doc_id).to_be_bytes());

    key
}

pub fn doc_reverse_bucket_prefix(collection_hash: u32, bucket_hash: u32) -> Vec<u8> {
    pair_bucket_prefix(INDEX_DOC_REVERSE, collection_hash, bucket_hash)
}

pub fn sortable_doc_id(doc_id: i32) -> u32 {
    (doc_id as u32) ^ 0x80000000
}

pub fn encode_doc_ids(doc_ids: &[i32]) -> Vec<u8> {
    let mut value = Vec::with_capacity(doc_ids.len() * 4);

    for doc_id in doc_ids {
        value.extend_from_slice(&sortable_doc_id(*doc_id).to_be_bytes());
    }

    value
}

pub fn decode_doc_ids(value: &[u8]) -> Option<Vec<i32>> {
    if !value.len().is_multiple_of(4) {
        return None;
    }

    Some(
        value
            .chunks_exact(4)
            .map(|chunk| {
                let sortable =
                    u32::from_be_bytes([chunk[0], chunk[1], chunk[2], chunk[3]]) ^ 0x80000000;

                sortable as i32
            })
            .collect(),
    )
}

pub fn encode_reverse_refs(refs: &[PairSignatureRef]) -> Vec<u8> {
    let count = refs.len().min(u16::MAX as usize);
    let mut value = Vec::with_capacity(2 + (count * 20));

    value.extend_from_slice(&(count as u16).to_be_bytes());

    for signature_ref in refs.iter().take(count) {
        value.push(signature_ref.index);
        value.push(signature_ref.pair_kind as u8);
        encode_word_slot(signature_ref.word1, &mut value);
        encode_word_slot(signature_ref.word2, &mut value);
    }

    value
}

pub fn decode_reverse_refs(value: &[u8]) -> Option<Vec<PairSignatureRef>> {
    if value.len() < 2 {
        return None;
    }

    let count = u16::from_be_bytes([value[0], value[1]]) as usize;
    let expected_len = 2 + (count * 20);

    if value.len() != expected_len {
        return None;
    }

    let mut refs = Vec::with_capacity(count);
    let mut offset = 2;

    for _ in 0..count {
        let index = value[offset];
        let pair_kind = decode_pair_kind(value[offset + 1])?;
        let word1 = decode_word_slot(&value[offset + 2..offset + 11])?;
        let word2 = decode_word_slot(&value[offset + 11..offset + 20])?;

        refs.push(PairSignatureRef {
            index,
            pair_kind,
            word1,
            word2,
        });

        offset += 20;
    }

    Some(refs)
}

pub fn letter_pair_code(first: char, second: char) -> u8 {
    debug_assert!(first.is_ascii_lowercase());
    debug_assert!(second == '$' || second.is_ascii_lowercase());

    let mut code = 0u8;

    for (row_first, groups) in LETTER_PAIR_ROWS {
        if row_first.as_bytes()[0] as char == first {
            for group in groups {
                if group.contains(second) {
                    return code;
                }

                code += 1;
            }
        } else {
            code += groups.len() as u8;
        }
    }

    unreachable!("letter-pair table must cover all ASCII lowercase pairs")
}

pub fn compatible_second_letter_codes(first: char, second: char) -> Vec<u8> {
    let mut code = 0u8;
    let mut codes = Vec::new();

    for (row_first, groups) in LETTER_PAIR_ROWS {
        if row_first.as_bytes()[0] as char == first {
            for group in groups {
                if group.contains(second) {
                    codes.push(code);
                }

                code += 1;
            }

            return codes;
        }

        code += groups.len() as u8;
    }

    codes
}

pub fn row_pair_codes(first: char) -> Vec<u8> {
    let mut code = 0u8;

    for (row_first, groups) in LETTER_PAIR_ROWS {
        if row_first.as_bytes()[0] as char == first {
            return (code..code + groups.len() as u8).collect();
        }

        code += groups.len() as u8;
    }

    Vec::new()
}

pub fn numeric_bucket(number: u64) -> u8 {
    let mut best_code = 0;
    let mut best_distance = number;

    for code in 0..=254 {
        let boundary = numeric_boundary(code);
        let distance = number.abs_diff(boundary);

        if distance < best_distance {
            best_code = code;
            best_distance = distance;
        }
    }

    best_code
}

pub fn numeric_boundary(code: u8) -> u64 {
    match code {
        0..=80 => u64::from(code),
        81..=120 => 80 + (u64::from(code - 80) * 5),
        121..=180 => 280 + (u64::from(code - 120) * 15),
        181..=220 => 1180 + (u64::from(code - 180) * 25),
        221..=240 => 3180 + (u64::from(code - 220) * 50),
        241..=254 => 8180 + (u64::from(code - 240) * 100),
        255 => unreachable!("255 is reserved for NUM_FLAG"),
    }
}

pub fn pack_u32(units: &[u8]) -> u32 {
    let mut buffer = [0; 4];

    for (index, unit) in units.iter().take(4).enumerate() {
        buffer[index] = *unit;
    }

    u32::from_be_bytes(buffer)
}

pub fn pack_u64(units: &[u8]) -> u64 {
    let mut buffer = [0; 8];

    for (index, unit) in units.iter().take(8).enumerate() {
        buffer[index] = *unit;
    }

    u64::from_be_bytes(buffer)
}

fn bucket_number(number: &str) -> u8 {
    numeric_bucket(number.parse::<u64>().unwrap_or(u64::MAX))
}

fn pair_bucket_prefix(index: u8, collection_hash: u32, bucket_hash: u32) -> Vec<u8> {
    let mut key = Vec::with_capacity(9);

    key.push(index);
    key.extend_from_slice(&collection_hash.to_be_bytes());
    key.extend_from_slice(&bucket_hash.to_be_bytes());

    key
}

fn encode_word_slot(slot: WordSlot, output: &mut Vec<u8>) {
    match slot {
        WordSlot::Short(signature) => {
            output.push(1);
            output.extend_from_slice(&(u64::from(signature) << 32).to_be_bytes());
        }
        WordSlot::Long(signature) => {
            output.push(2);
            output.extend_from_slice(&signature.to_be_bytes());
        }
    }
}

fn decode_word_slot(value: &[u8]) -> Option<WordSlot> {
    if value.len() != 9 {
        return None;
    }

    let signature = u64::from_be_bytes([
        value[1], value[2], value[3], value[4], value[5], value[6], value[7], value[8],
    ]);

    match value[0] {
        1 => Some(WordSlot::Short((signature >> 32) as u32)),
        2 => Some(WordSlot::Long(signature)),
        _ => None,
    }
}

fn decode_pair_kind(value: u8) -> Option<PairKind> {
    match value {
        1 => Some(PairKind::Adjacent),
        2 => Some(PairKind::Wrap),
        _ => None,
    }
}

fn normalize_char(source: char) -> Option<char> {
    match source {
        'a'..='z' | '0'..='9' => Some(source),
        'A'..='Z' => Some(source.to_ascii_lowercase()),
        'á' | 'à' | 'ä' | 'â' | 'Á' | 'À' | 'Ä' | 'Â' => Some('a'),
        'é' | 'è' | 'ë' | 'ê' | 'É' | 'È' | 'Ë' | 'Ê' => Some('e'),
        'í' | 'ì' | 'ï' | 'î' | 'Í' | 'Ì' | 'Ï' | 'Î' => Some('i'),
        'ó' | 'ò' | 'ö' | 'ô' | 'Ó' | 'Ò' | 'Ö' | 'Ô' => Some('o'),
        'ú' | 'ù' | 'ü' | 'û' | 'Ú' | 'Ù' | 'Ü' | 'Û' => Some('u'),
        'ñ' | 'Ñ' => Some('n'),
        _ => None,
    }
}

impl EncodedWord {
    pub fn slot(&self) -> WordSlot {
        match self.signature {
            WordSignature::Short(signature) => WordSlot::Short(signature),
            WordSignature::Long(signature) => WordSlot::Long(signature),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn it_normalizes_spanish_product_text() {
        assert_eq!(normalize("CAÑA 100LTS"), vec!["cana", "100lts"]);
        assert_eq!(normalize("Arroz--Extra"), vec!["aroz", "extra"]);
        assert_eq!(normalize("COOOLL"), vec!["col"]);
    }

    #[test]
    fn it_assigns_deterministic_letter_pair_codes() {
        assert_eq!(letter_pair_code('a', 'r'), 0);
        assert_eq!(letter_pair_code('a', '$'), 24);
        assert_eq!(letter_pair_code('b', 'o'), 25);
        assert_eq!(letter_pair_code('z', '$'), 249);
        assert_ne!(letter_pair_code('z', '$'), NUM_FLAG);
    }

    #[test]
    fn it_encodes_odd_trailing_letters() {
        let sol = encode_text("sol").remove(0);

        assert_eq!(
            sol.units,
            vec![letter_pair_code('s', 'o'), letter_pair_code('l', '$')]
        );
        assert_eq!(sol.signature, WordSignature::Short(pack_u32(&sol.units)));

        let a = encode_text("a").remove(0);

        assert_eq!(a.units, vec![letter_pair_code('a', '$')]);
    }

    #[test]
    fn it_encodes_numeric_runs_as_two_units() {
        let encoded = encode_text("100lts").remove(0);

        assert_eq!(encoded.units[0], NUM_FLAG);
        assert_eq!(encoded.units[1], numeric_bucket(100));
        assert_eq!(encoded.units[2], letter_pair_code('l', 't'));
        assert_eq!(encoded.units[3], letter_pair_code('s', '$'));
        assert_eq!(
            encoded.signature,
            WordSignature::Short(pack_u32(&encoded.units))
        );
    }

    #[test]
    fn it_quantizes_numbers_to_nearest_bucket() {
        assert_eq!(numeric_bucket(0), 0);
        assert_eq!(numeric_bucket(80), 80);
        assert_eq!(numeric_bucket(83), 81);
        assert_eq!(numeric_bucket(85), 81);
        assert_eq!(numeric_bucket(9580), 254);
        assert_eq!(numeric_bucket(20_000), 254);
    }

    #[test]
    fn it_packs_short_and_long_signatures_big_endian() {
        assert_eq!(pack_u32(&[1, 2, 3]), 0x01020300);
        assert_eq!(pack_u64(&[1, 2, 3, 4, 5]), 0x0102030405000000);

        assert!(matches!(
            encode_text("tequila").remove(0).signature,
            WordSignature::Short(_)
        ));
        assert!(matches!(
            encode_text("extraordinario").remove(0).signature,
            WordSignature::Long(_)
        ));
    }

    #[test]
    fn it_expands_single_letter_prefix_pair_groups() {
        assert_eq!(
            compatible_second_letter_codes('n', 'a'),
            vec![letter_pair_code('n', 'a')]
        );
        assert_eq!(
            compatible_second_letter_codes('n', '$'),
            vec![letter_pair_code('n', '$')]
        );
    }

    #[test]
    fn it_encodes_open_prefix_without_end_marker() {
        let prefix = encode_prefix_token("sunr").unwrap();

        assert_eq!(
            prefix.unit_prefixes,
            vec![vec![letter_pair_code('s', 'u'), letter_pair_code('n', 'r')]]
        );

        let (complete, prefix) = encode_prefix_text("tequila sunr*");

        assert_eq!(complete.len(), 1);
        assert_eq!(prefix.unwrap().token, "sunr");
    }

    #[test]
    fn it_expands_trailing_single_letter_prefixes() {
        let prefix = encode_prefix_token("sun").unwrap();

        assert_eq!(prefix.unit_prefixes.len(), row_pair_codes('n').len());
        assert!(prefix.unit_prefixes.contains(&vec![
            letter_pair_code('s', 'u'),
            letter_pair_code('n', 'r')
        ]));

        let prefix = encode_prefix_token("100l").unwrap();

        assert!(prefix.unit_prefixes.contains(&vec![
            NUM_FLAG,
            numeric_bucket(100),
            letter_pair_code('l', 't')
        ]));
    }

    #[test]
    fn it_generates_adjacent_and_wrap_pair_entries() {
        let words = encode_text("agua mineral gas");
        let entries = pair_entries(&words);

        assert_eq!(entries.len(), 3);
        assert_eq!(entries[0].pair_kind, PairKind::Adjacent);
        assert_eq!(entries[1].pair_kind, PairKind::Adjacent);
        assert_eq!(entries[2].pair_kind, PairKind::Wrap);
        assert_eq!(entries[2].word1, words[2].slot());
        assert_eq!(entries[2].word2, words[0].slot());

        let one_word = encode_text("tequila");
        let entries = pair_entries(&one_word);

        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].pair_kind, PairKind::Wrap);
        assert_eq!(entries[0].word1, one_word[0].slot());
        assert_eq!(entries[0].word2, one_word[0].slot());
    }

    #[test]
    fn it_builds_lexicographic_storage_keys() {
        assert_eq!(
            compact_pair_key(
                0x01020304,
                0x05060708,
                0x11223344,
                0x55667788,
                PairKind::Adjacent
            ),
            vec![
                INDEX_COMPACT_PAIR,
                0x01,
                0x02,
                0x03,
                0x04,
                0x05,
                0x06,
                0x07,
                0x08,
                0x11,
                0x22,
                0x33,
                0x44,
                0x55,
                0x66,
                0x77,
                0x88,
                PairKind::Adjacent as u8,
            ]
        );

        assert_eq!(
            compact_pair_bucket_prefix(0x01020304, 0x05060708),
            vec![
                INDEX_COMPACT_PAIR,
                0x01,
                0x02,
                0x03,
                0x04,
                0x05,
                0x06,
                0x07,
                0x08,
            ]
        );

        assert_eq!(
            compact_pair_first_word_prefix(0x01020304, 0x05060708, 0x11223344),
            vec![
                INDEX_COMPACT_PAIR,
                0x01,
                0x02,
                0x03,
                0x04,
                0x05,
                0x06,
                0x07,
                0x08,
                0x11,
                0x22,
                0x33,
                0x44,
            ]
        );

        assert_eq!(
            doc_reverse_key(0x01020304, 0x05060708, -1),
            vec![
                INDEX_DOC_REVERSE,
                0x01,
                0x02,
                0x03,
                0x04,
                0x05,
                0x06,
                0x07,
                0x08,
                0x7f,
                0xff,
                0xff,
                0xff,
            ]
        );
    }

    #[test]
    fn it_round_trips_doc_id_posting_values() {
        let doc_ids = vec![i32::MIN, -10, -1, 0, 1, 42, i32::MAX];
        let encoded = encode_doc_ids(&doc_ids);

        assert_eq!(decode_doc_ids(&encoded), Some(doc_ids));
        assert_eq!(decode_doc_ids(&encoded[0..encoded.len() - 1]), None);
    }

    #[test]
    fn it_round_trips_fixed_width_reverse_refs() {
        let refs = vec![
            PairSignatureRef {
                index: INDEX_COMPACT_PAIR,
                pair_kind: PairKind::Adjacent,
                word1: WordSlot::Short(0x11223344),
                word2: WordSlot::Short(0x55667788),
            },
            PairSignatureRef {
                index: INDEX_EXTENDED_PAIR,
                pair_kind: PairKind::Wrap,
                word1: WordSlot::Long(0x1122334455667788),
                word2: WordSlot::Short(0xaabbccdd),
            },
        ];
        let encoded = encode_reverse_refs(&refs);

        assert_eq!(decode_reverse_refs(&encoded), Some(refs));
        assert_eq!(decode_reverse_refs(&encoded[0..encoded.len() - 1]), None);
    }
}
