// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use crate::store::item::StoreItem;
use crate::store::kv::StoreKVActionBuilder;

impl super::Executor {
    pub fn flushc(&self, store: StoreItem) -> Result<u32, ()> {
        if let StoreItem(collection, None, None) = store {
            general_kv_access_lock_write!();

            let kv_action_builder = StoreKVActionBuilder {
                kv_pool: &self.kv_pool,
            };

            kv_action_builder.erase(collection, None)
        } else {
            Err(())
        }
    }
}
