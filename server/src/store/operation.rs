// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use crate::{Executor, Query};

pub struct StoreOperationDispatch;

impl StoreOperationDispatch {
    pub fn dispatch(query: Query, executor: &Executor) -> Result<Option<String>, ()> {
        match query {
            Query::Search(store, query_id, terms, limit, offset) => executor
                .search(store, query_id, terms, limit, offset)
                .map(|results| results.map(|results| results.join(" "))),
            Query::PushI(store, doc_id, text) => executor.pushi(store, doc_id, text).map(|_| None),
            Query::PopI(store, doc_id) => executor
                .popi(store, doc_id)
                .map(|count| Some(count.to_string())),
            Query::FlushC(store) => executor.flushc(store).map(|count| Some(count.to_string())),
            Query::FlushB(store) => executor.flushb(store).map(|count| Some(count.to_string())),
        }
    }
}
