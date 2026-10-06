// GenixSearch — ranking tunables
//
// Every score the query path can add to a document lives here. Tune by
// changing the constants; nothing else needs to be touched.

// ---- Query-side position bonuses ---------------------------------------
//
// Applied to single-word matches based on the position of the token in the
// *query*. The first query word gets the largest bonus so the user's primary
// intent dominates accidental matches on trailing tokens.

pub const QUERY_FIRST_WORD_BONUS: u32 = 3;
pub const QUERY_SECOND_WORD_BONUS: u32 = 1;

// ---- Doc-side position bonuses -----------------------------------------
//
// Applied when the matched token's position *inside the indexed document*
// is at the front of the document. This is what lifts "Pimienta negra
// molida" above "Sal y pimienta de cocina" when the query is "pimienta".

pub const DOC_FIRST_WORD_BONUS: u32 = 4;
pub const DOC_SECOND_WORD_BONUS: u32 = 2;

// ---- Pair-match scores -------------------------------------------------
//
// Earned by a query adjacent pair (X, Y) when the indexed document also
// contains that pair. Adjacent = doc has X→Y in order; Wrap = doc has Y→X
// (the wrap-pair encoding stores the reversed phrase).

pub const ADJACENT_PAIR_SCORE: u32 = 5;
pub const WRAP_PAIR_SCORE: u32 = 1;

// ---- Prefix-match score ------------------------------------------------
//
// Earned by a doc whose word matches the open-prefix query (`sunr*`) or
// the auto-prefix derived from the last complete query word.

pub const PREFIX_SCORE: u32 = 5;

// ---- Single-word base score --------------------------------------------
//
// Length-weighted base for matching a complete query token. A 2-letter
// token earns the floor, an 8+-letter token earns the ceiling. Adjust
// `BASE`, `FLOOR`, `CEIL` together — `BASE + len` is clamped between
// `FLOOR` and `CEIL`.

pub const WORD_BASE_SCORE_BASE: u32 = 6;
pub const WORD_BASE_SCORE_FLOOR: u32 = 8;
pub const WORD_BASE_SCORE_CEIL: u32 = 12;

// ------------------------------------------------------------------------
// Helpers that read the constants above. Code outside this module should
// call these, not duplicate the formulas.

pub fn word_base_score(token: &str) -> u32 {
    let len = token.chars().count() as u32;

    (WORD_BASE_SCORE_BASE + len).clamp(WORD_BASE_SCORE_FLOOR, WORD_BASE_SCORE_CEIL)
}

pub fn query_position_bonus(position: usize) -> u32 {
    match position {
        0 => QUERY_FIRST_WORD_BONUS,
        1 => QUERY_SECOND_WORD_BONUS,
        _ => 0,
    }
}

pub fn doc_position_bonus(pos: u8) -> u32 {
    match pos {
        0 => DOC_FIRST_WORD_BONUS,
        1 => DOC_SECOND_WORD_BONUS,
        _ => 0,
    }
}
