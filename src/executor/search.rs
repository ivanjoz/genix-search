// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use hashbrown::{HashMap, HashSet};

use super::scoring::{
    ADJACENT_PAIR_SCORE, PREFIX_SCORE, QUERY_FIRST_WORD_BONUS, WRAP_PAIR_SCORE,
    doc_position_bonus, query_position_bonus, word_base_score,
};
use crate::pair::{PairEntry, PairKind, PostingMeta, encode_prefix_text};
use crate::query::{QuerySearchID, QuerySearchLimit, QuerySearchOffset};
use crate::store::item::StoreItem;
use crate::store::kv::{StoreKVAcquireMode, StoreKVActionBuilder};

#[derive(Default)]
struct DirectScore {
    score: u32,
    pair_matches: u32,
    single_matches: u32,
}

impl super::Executor {
    pub fn search<'a>(
        &self,
        store: StoreItem<'a>,
        _event_id: QuerySearchID,
        terms: &'a str,
        limit: QuerySearchLimit,
        offset: QuerySearchOffset,
    ) -> Result<Option<Vec<String>>, ()> {
        if let StoreItem(collection, Some(bucket), None) = store {
            general_kv_access_lock_read!();

            if let Ok(kv_store) = self
                .kv_pool
                .acquire(StoreKVAcquireMode::OpenOnly, collection)
            {
                executor_kv_lock_read!(kv_store);

                let kv_action = StoreKVActionBuilder::access(bucket, kv_store);
                let (words, prefix) = encode_prefix_text(terms);

                if words.is_empty() && prefix.is_none() {
                    return Ok(None);
                }

                let candidate_budget = usize::from(limit).saturating_mul(10).clamp(500, 5000);
                let mut scores: HashMap<i32, DirectScore> = HashMap::new();
                let mut last_word_doc_ids: HashSet<i32> = HashSet::new();

                for (position, word) in words.iter().enumerate() {
                    let mut best_positions: HashMap<i32, u8> = HashMap::new();

                    for (doc_id, meta_byte) in kv_action
                        .scan_pair_postings_by_first_word(word.slot(), candidate_budget)?
                    {
                        let w1_pos = PostingMeta::unpack(meta_byte).w1_pos;

                        best_positions
                            .entry(doc_id)
                            .and_modify(|current| {
                                if w1_pos < *current {
                                    *current = w1_pos;
                                }
                            })
                            .or_insert(w1_pos);
                    }

                    let delta = word_base_score(&word.token) + query_position_bonus(position);

                    for (doc_id, best_pos) in &best_positions {
                        let score = scores.entry(*doc_id).or_default();

                        score.score += delta + doc_position_bonus(*best_pos);
                        score.single_matches += 1;
                    }

                    if position + 1 == words.len() {
                        last_word_doc_ids = best_positions.keys().copied().collect();
                    }
                }

                if let Some(prefix) = prefix {
                    let mut prefix_best_positions: HashMap<i32, u8> = HashMap::new();
                    let use_second_word_pos = prefix.explicit && words.last().is_some();

                    for unit_prefix in prefix.unit_prefixes {
                        let postings = match (prefix.explicit, words.last()) {
                            (true, Some(last_word)) => kv_action
                                .scan_pair_postings_by_second_word_units(
                                    last_word.slot(),
                                    &unit_prefix,
                                    candidate_budget,
                                )?,
                            _ => kv_action.scan_pair_postings_by_first_word_units(
                                &unit_prefix,
                                candidate_budget,
                            )?,
                        };

                        for (doc_id, meta_byte) in postings {
                            let meta = PostingMeta::unpack(meta_byte);
                            let pos = if use_second_word_pos {
                                meta.w2_pos
                            } else {
                                meta.w1_pos
                            };

                            prefix_best_positions
                                .entry(doc_id)
                                .and_modify(|current| {
                                    if pos < *current {
                                        *current = pos;
                                    }
                                })
                                .or_insert(pos);
                        }
                    }

                    let prefix_bonus = if words.is_empty() || (!prefix.explicit && words.len() == 1)
                    {
                        QUERY_FIRST_WORD_BONUS
                    } else {
                        0
                    };

                    for (doc_id, best_pos) in &prefix_best_positions {
                        if !prefix.explicit && last_word_doc_ids.contains(doc_id) {
                            continue;
                        }

                        let score = scores.entry(*doc_id).or_default();

                        score.score += PREFIX_SCORE + prefix_bonus + doc_position_bonus(*best_pos);
                        score.pair_matches += 1;
                    }

                    if prefix.explicit {
                        let prefix_doc_ids: HashSet<i32> =
                            prefix_best_positions.keys().copied().collect();

                        scores.retain(|doc_id, _| prefix_doc_ids.contains(doc_id));
                    }
                }

                for window in words.windows(2) {
                    let entry = PairEntry {
                        word1: window[0].slot(),
                        word2: window[1].slot(),
                    };

                    let Some(postings) = kv_action.get_pair_postings(entry)? else {
                        continue;
                    };

                    for (doc_id, meta_byte) in postings {
                        let meta = PostingMeta::unpack(meta_byte);
                        let delta = match meta.kind {
                            PairKind::Adjacent => ADJACENT_PAIR_SCORE,
                            PairKind::Wrap => WRAP_PAIR_SCORE,
                        };
                        let score = scores.entry(doc_id).or_default();

                        score.score += delta;
                        score.pair_matches += 1;
                    }
                }

                let mut ranked: Vec<(i32, DirectScore)> = scores.into_iter().collect();

                ranked.sort_by(|(left_doc_id, left), (right_doc_id, right)| {
                    right
                        .score
                        .cmp(&left.score)
                        .then_with(|| right.pair_matches.cmp(&left.pair_matches))
                        .then_with(|| right.single_matches.cmp(&left.single_matches))
                        .then_with(|| left_doc_id.cmp(right_doc_id))
                });

                let result_ids: Vec<String> = ranked
                    .into_iter()
                    .skip(offset as usize)
                    .take(limit as usize)
                    .map(|(doc_id, score)| format!("{}|{}", doc_id, score.score))
                    .collect();

                info!("got direct search executor final ids: {:?}", result_ids);

                return Ok(if !result_ids.is_empty() {
                    Some(result_ids)
                } else {
                    None
                });
            }
        }

        Err(())
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use crate::query::Query;
    use crate::store::kv::StoreKVPool;
    use crate::store::operation::StoreOperationDispatch;

    use super::super::Executor;

    #[test]
    fn it_ranks_direct_pair_query_results() {
        let executor = test_executor();
        let collection = "c:test:direct-search";
        let bucket = "b:test:direct-search";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "1", "tequila sunrise").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "2", "tequila promo sunrise").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-search",
                collection,
                bucket,
                "tequila sunrise",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        assert!(result.starts_with("1|39"));
        assert!(result.contains("2|32"));
    }

    #[test]
    fn it_finds_direct_pair_prefix_query_results() {
        let executor = test_executor();
        let collection = "c:test:direct-prefix-search";
        let bucket = "b:test:direct-prefix-search";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "10", "tequila sunrise").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "20", "tequila sunset").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-prefix-search",
                collection,
                bucket,
                "tequila sunr*",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        assert!(result.starts_with("10|26"));
        assert!(!result.contains("20|"));
    }

    #[test]
    fn it_ranks_reverse_order_pair_below_adjacent() {
        let executor = test_executor();
        let collection = "c:test:direct-wrap-rank";
        let bucket = "b:test:direct-wrap-rank";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "1", "tequila sunrise").unwrap(),
            &executor,
        )
        .unwrap();

        let adjacent = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-wrap-rank-fwd",
                collection,
                bucket,
                "tequila sunrise",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        let reversed = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-wrap-rank-rev",
                collection,
                bucket,
                "sunrise tequila",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        assert!(adjacent.starts_with("1|39"));
        assert!(reversed.starts_with("1|35"));
    }

    #[test]
    fn it_finds_direct_numeric_letter_prefix_query_results() {
        let executor = test_executor();
        let collection = "c:test:direct-numeric-prefix-search";
        let bucket = "b:test:direct-numeric-prefix-search";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "100", "100lts").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "200", "100ml").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-numeric-prefix-search",
                collection,
                bucket,
                "100l*",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        assert_eq!(result, "100|12");
    }

    #[test]
    fn it_orders_results_by_descending_score() {
        let executor = test_executor();
        let collection = "c:test:direct-order";
        let bucket = "b:test:direct-order";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "1", "agua mineral").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "2", "agua sin gas").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "3", "mineral con gas").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-order",
                collection,
                bucket,
                "agua mineral",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        let scores: Vec<u32> = result
            .split_whitespace()
            .filter_map(|pair| pair.split('|').nth(1).and_then(|s| s.parse().ok()))
            .collect();

        assert!(!scores.is_empty());
        assert!(scores.windows(2).all(|window| window[0] >= window[1]));
        assert!(result.starts_with("1|"));
    }

    #[test]
    fn it_indexes_signed_i32_doc_keys_without_translation() {
        let executor = test_executor();
        let collection = "c:test:direct-signed";
        let bucket = "b:test:direct-signed";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, &i32::MIN.to_string(), "agua mineral").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "-1", "agua sin gas").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, &i32::MAX.to_string(), "agua con gas").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-signed",
                collection,
                bucket,
                "agua",
                10,
                0,


            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        assert!(result.contains(&format!("{}|", i32::MIN)));
        assert!(result.contains("-1|"));
        assert!(result.contains(&format!("{}|", i32::MAX)));
    }

    #[test]
    fn it_ranks_first_query_word_above_trailing_stopword_matches() {
        let executor = test_executor();
        let collection = "c:test:direct-first-word";
        let bucket = "b:test:direct-first-word";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "1", "pimienta negra molida").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "2", "bateria de cocina").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-first-word",
                collection,
                bucket,
                "pimienta roja de cocina",
                10,
                0,
            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        let first_id = result
            .split_whitespace()
            .next()
            .and_then(|token| token.split('|').next())
            .expect("at least one result");
        assert_eq!(first_id, "1");
    }

    #[test]
    fn it_auto_prefix_matches_last_query_word_partial() {
        let executor = test_executor();
        let collection = "c:test:direct-auto-prefix";
        let bucket = "b:test:direct-auto-prefix";

        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "1", "pimienta negra molida").unwrap(),
            &executor,
        )
        .unwrap();
        StoreOperationDispatch::dispatch(
            Query::pushi(collection, bucket, "2", "pimienta de cerdo").unwrap(),
            &executor,
        )
        .unwrap();

        let result = StoreOperationDispatch::dispatch(
            Query::search(
                "query:test:direct-auto-prefix",
                collection,
                bucket,
                "pimienta moli",
                10,
                0,
            )
            .unwrap(),
            &executor,
        )
        .unwrap()
        .unwrap();

        let tokens: Vec<&str> = result.split_whitespace().collect();
        assert!(
            tokens.iter().any(|t| t.starts_with("1|")),
            "doc 1 (with `molida`) should appear: {}",
            result
        );
        assert!(
            tokens.iter().any(|t| t.starts_with("2|")),
            "doc 2 (without `moli*`) should still appear: {}",
            result
        );

        let score = |prefix: &str| -> u32 {
            tokens
                .iter()
                .find(|t| t.starts_with(prefix))
                .and_then(|t| t.split('|').nth(1))
                .and_then(|s| s.parse().ok())
                .unwrap_or(0)
        };
        assert!(
            score("1|") > score("2|"),
            "doc 1 (molida) must outrank doc 2 (no moli*): {}",
            result
        );
    }

    fn test_executor() -> Executor {
        let app_conf = Arc::new(
            config::Config::builder()
                .add_source(config::File::from_str(
                    crate::config::tests::defaults_toml(),
                    config::FileFormat::Toml,
                ))
                .build()
                .unwrap()
                .try_deserialize::<crate::Config>()
                .unwrap(),
        );

        Executor {
            kv_pool: StoreKVPool::new(Arc::clone(&app_conf.store.kv)),
            app_conf,
        }
    }
}
