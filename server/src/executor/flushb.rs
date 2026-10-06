// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use crate::store::item::StoreItem;
use crate::store::kv::{StoreKVAcquireMode, StoreKVActionBuilder};

impl super::Executor {
    pub fn flushb(&self, store: StoreItem) -> Result<u32, ()> {
        if let StoreItem(collection, Some(bucket), None) = store {
            general_kv_access_lock_read!();

            if let Ok(kv_store) = self
                .kv_pool
                .acquire(StoreKVAcquireMode::OpenOnly, collection)
            {
                executor_kv_lock_write!(kv_store);

                if kv_store.is_some() {
                    debug!(
                        "collection store exists, erasing: {} from {}",
                        bucket.as_str(),
                        collection.as_str()
                    );

                    let kv_action = StoreKVActionBuilder::access(bucket, kv_store);

                    return kv_action.batch_erase_bucket();
                } else {
                    debug!(
                        "collection store does not exist, consider {} from {} already erased",
                        bucket.as_str(),
                        collection.as_str()
                    );

                    return Ok(0);
                }
            }
        }

        Err(())
    }
}
