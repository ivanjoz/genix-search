// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

mod builder;
mod types;

use crate::store::item::StoreItem;

pub use self::types::*;

pub enum Query<'a> {
    Search(
        StoreItem<'a>,
        QuerySearchID<'a>,
        &'a str,
        QuerySearchLimit,
        QuerySearchOffset,
    ),
    PushI(StoreItem<'a>, i32, &'a str),
    PopI(StoreItem<'a>, i32),
    FlushC(StoreItem<'a>),
    FlushB(StoreItem<'a>),
}
