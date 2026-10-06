// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use super::Query;
use super::types::{QuerySearchLimit, QuerySearchOffset};
use crate::store::item::StoreItemBuilder;

impl<'a> Query<'a> {
    pub fn search(
        query_id: &'a str,
        collection: &'a str,
        bucket: &'a str,
        terms: &'a str,
        limit: QuerySearchLimit,
        offset: QuerySearchOffset,
    ) -> Result<Self, ()> {
        match StoreItemBuilder::from_depth_2(collection, bucket) {
            Ok(store) => Ok(Query::Search(store, query_id, terms, limit, offset)),
            _ => Err(()),
        }
    }

    pub fn pushi(
        collection: &'a str,
        bucket: &'a str,
        object: &'a str,
        text: &'a str,
    ) -> Result<Self, ()> {
        match (
            StoreItemBuilder::from_depth_2(collection, bucket),
            object.parse::<i32>(),
        ) {
            (Ok(store), Ok(doc_id)) => Ok(Query::PushI(store, doc_id, text)),
            _ => Err(()),
        }
    }

    pub fn popi(collection: &'a str, bucket: &'a str, object: &'a str) -> Result<Self, ()> {
        match (
            StoreItemBuilder::from_depth_2(collection, bucket),
            object.parse::<i32>(),
        ) {
            (Ok(store), Ok(doc_id)) => Ok(Query::PopI(store, doc_id)),
            _ => Err(()),
        }
    }

    pub fn flushc(collection: &'a str) -> Result<Self, ()> {
        match StoreItemBuilder::from_depth_1(collection) {
            Ok(store) => Ok(Query::FlushC(store)),
            _ => Err(()),
        }
    }

    pub fn flushb(collection: &'a str, bucket: &'a str) -> Result<Self, ()> {
        match StoreItemBuilder::from_depth_2(collection, bucket) {
            Ok(store) => Ok(Query::FlushB(store)),
            _ => Err(()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn it_builds_search_query() {
        assert!(Query::search("id1", "c:test:1", "b:test:1", "Michael Dake", 10, 20).is_ok());
        assert!(Query::search("id2", "c:test:1", "", "Michael Dake", 1, 0).is_err());
    }

    #[test]
    fn it_builds_pushi_query() {
        assert!(Query::pushi("c:test:3i", "b:test:3i", "-42", "Tequila sunrise").is_ok());
        assert!(Query::pushi("c:test:3i", "b:test:3i", "bad", "Tequila sunrise").is_err());
        assert!(Query::pushi("c:test:3i", "", "42", "Tequila sunrise").is_err());
    }

    #[test]
    fn it_builds_popi_query() {
        assert!(Query::popi("c:test:4i", "b:test:4i", "-42").is_ok());
        assert!(Query::popi("c:test:4i", "b:test:4i", "bad").is_err());
        assert!(Query::popi("c:test:4i", "", "42").is_err());
    }

    #[test]
    fn it_builds_flushc_query() {
        assert!(Query::flushc("c:test:6").is_ok());
        assert!(Query::flushc("").is_err());
    }

    #[test]
    fn it_builds_flushb_query() {
        assert!(Query::flushb("c:test:7", "b:test:7").is_ok());
        assert!(Query::flushb("c:test:7", "").is_err());
    }
}
