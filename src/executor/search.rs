// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use hashbrown::{HashMap, HashSet};

use crate::pair::{PairEntry, PairKind, WordSignature, encode_prefix_text, pair_entries};
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

                for word in &words {
                    let doc_ids =
                        kv_action.scan_pair_doc_ids_by_first_word(word.slot(), candidate_budget)?;

                    for doc_id in doc_ids {
                        let score = scores.entry(doc_id).or_default();

                        score.score += match word.signature {
                            WordSignature::Short(_) => 10,
                            WordSignature::Long(_) => 12,
                        };
                        score.single_matches += 1;
                    }
                }

                if let Some(prefix) = prefix {
                    let mut prefix_doc_ids = HashSet::new();

                    for unit_prefix in prefix.unit_prefixes {
                        let doc_ids = if let Some(last_word) = words.last() {
                            kv_action.scan_pair_doc_ids_by_second_word_units(
                                last_word.slot(),
                                &unit_prefix,
                                candidate_budget,
                            )?
                        } else {
                            kv_action.scan_pair_doc_ids_by_first_word_units(
                                &unit_prefix,
                                candidate_budget,
                            )?
                        };

                        prefix_doc_ids.extend(doc_ids);
                    }

                    for doc_id in &prefix_doc_ids {
                        let score = scores.entry(*doc_id).or_default();

                        score.score += 15;
                        score.pair_matches += 1;
                    }

                    scores.retain(|doc_id, _| prefix_doc_ids.contains(doc_id));
                }

                for entry in pair_entries(&words)
                    .into_iter()
                    .filter(|entry| entry.pair_kind == PairKind::Adjacent)
                {
                    if let Some(doc_ids) = kv_action.get_pair_doc_ids(entry)? {
                        for doc_id in doc_ids {
                            let score = scores.entry(doc_id).or_default();

                            score.score += 30;
                            score.pair_matches += 1;
                        }
                    }

                    let wrap_entry = PairEntry {
                        pair_kind: PairKind::Wrap,
                        word1: entry.word1,
                        word2: entry.word2,
                    };

                    if let Some(doc_ids) = kv_action.get_pair_doc_ids(wrap_entry)? {
                        for doc_id in doc_ids {
                            let score = scores.entry(doc_id).or_default();

                            score.score += 6;
                            score.pair_matches += 1;
                        }
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

        assert!(result.starts_with("1|50"));
        assert!(result.contains("2|20"));
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

        assert!(result.starts_with("10|25"));
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

        assert!(adjacent.starts_with("1|50"));
        assert!(reversed.starts_with("1|26"));
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

        assert_eq!(result, "100|15");
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
