// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use std::sync::Arc;

#[macro_use]
mod macros;

mod direct;
mod flushb;
mod flushc;
mod search;

pub struct Executor {
    pub app_conf: Arc<crate::Config>,
    pub kv_pool: crate::store::kv::StoreKVPool,
}
