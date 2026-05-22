// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use crate::pair::{encode_text, pair_entries};
use crate::store::item::StoreItem;
use crate::store::kv::{StoreKVAcquireMode, StoreKVActionBuilder};

impl super::Executor {
    pub fn pushi<'a>(&self, store: StoreItem<'a>, doc_id: i32, text: &str) -> Result<(), ()> {
        if let StoreItem(collection, Some(bucket), None) = store {
            general_kv_access_lock_read!();

            if let Ok(kv_store) = self.kv_pool.acquire(StoreKVAcquireMode::Any, collection) {
                executor_kv_lock_write!(kv_store);

                let kv_action = StoreKVActionBuilder::access(bucket, kv_store);
                let words = encode_text(text);
                let entries = pair_entries(&words);

                kv_action.replace_direct_pair_document(doc_id, &entries)?;

                return Ok(());
            }
        }

        Err(())
    }

    pub fn popi<'a>(&self, store: StoreItem<'a>, doc_id: i32) -> Result<u32, ()> {
        if let StoreItem(collection, Some(bucket), None) = store {
            general_kv_access_lock_read!();

            if let Ok(kv_store) = self
                .kv_pool
                .acquire(StoreKVAcquireMode::OpenOnly, collection)
            {
                executor_kv_lock_write!(kv_store);

                let kv_action = StoreKVActionBuilder::access(bucket, kv_store);

                return kv_action.delete_direct_pair_document(doc_id);
            }
        }

        Err(())
    }
}
