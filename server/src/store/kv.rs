// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use hashbrown::HashMap;
use radix::RadixNum;
use rocksdb::backup::{
    BackupEngine as DBBackupEngine, BackupEngineOptions as DBBackupEngineOptions,
    RestoreOptions as DBRestoreOptions,
};
use rocksdb::{
    DB, DBCompactionStyle, DBCompressionType, Direction, Env as DBEnv, Error as DBError,
    FlushOptions, IteratorMode, Options as DBOptions, WriteBatch, WriteOptions,
};
use std::fmt;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, RwLock};
use std::thread;
use std::time::{Duration, SystemTime};

use crate::pair::{
    PairEntry, PairSignatureRef, POSTING_WIDTH, PostingMeta, WordSlot,
    compact_pair_bucket_prefix, compact_pair_first_word_prefix,
    compact_pair_first_word_unit_prefix, compact_pair_second_word_unit_prefix, decode_postings,
    decode_reverse_refs, doc_reverse_bucket_prefix, doc_reverse_key, encode_postings,
    encode_reverse_refs, extended_pair_bucket_prefix, extended_pair_first_word_prefix,
    extended_pair_first_word_unit_prefix, extended_pair_second_word_unit_prefix, pair_key,
    pair_signature_ref, sortable_doc_id,
};

use super::generic::{
    StoreGeneric, StoreGenericActionBuilder, StoreGenericBuilder, StoreGenericPool,
};
use super::item::StoreItemPart;

// NOTE: This type cannot be generic over a lifetime as spawning threads would
//   force it to be `'static`.
#[derive(Clone)]
pub struct StoreKVPool {
    pool: Arc<RwLock<HashMap<StoreKVKey, StoreKVBox>>>,
    kv_store_config: Arc<crate::config::ConfigStoreKV>,
}

pub struct StoreKVBuilder {
    kv_store_config: Arc<crate::config::ConfigStoreKV>,
}

pub struct StoreKV {
    database: DB,
    pub(crate) collection_hash: StoreKVAtom,
    last_used: Arc<RwLock<SystemTime>>,
    last_flushed: Arc<RwLock<SystemTime>>,
    pub lock: RwLock<bool>,
    kv_store_config: Arc<crate::config::ConfigStoreKV>,
}

pub struct StoreKVActionBuilder<'build> {
    pub kv_pool: &'build StoreKVPool,
}

pub struct StoreKVAction<'a> {
    store: Option<StoreKVBox>,
    bucket: StoreItemPart<'a>,
}

#[derive(PartialEq, Eq, Hash, Clone, Copy)]
pub struct StoreKVKey {
    collection_hash: StoreKVAtom,
}

#[derive(PartialEq)]
pub enum StoreKVAcquireMode {
    Any,
    OpenOnly,
}

pub(crate) type StoreKVAtom = u32;
type StoreKVBox = Arc<StoreKV>;

const ATOM_HASH_RADIX: usize = 16;

lazy_static! {
    pub(crate) static ref STORE_ACCESS_LOCK: Arc<RwLock<bool>> = Arc::new(RwLock::new(false));
    static ref STORE_ACQUIRE_LOCK: Arc<Mutex<()>> = Arc::new(Mutex::new(()));
    static ref STORE_FLUSH_LOCK: Arc<Mutex<()>> = Arc::new(Mutex::new(()));
}

impl StoreKVPool {
    pub fn new(kv_store_config: Arc<crate::config::ConfigStoreKV>) -> Self {
        Self {
            pool: Arc::default(),
            kv_store_config,
        }
    }

    pub fn count(&self) -> usize {
        self.pool.read().unwrap().len()
    }

    pub fn acquire<'a, T: Into<&'a str>>(
        &self,
        mode: StoreKVAcquireMode,
        collection: T,
    ) -> Result<Option<StoreKVBox>, ()> {
        let collection_str = collection.into();
        let pool_key = StoreKVKey::from_str(collection_str);

        let _acquire = STORE_ACQUIRE_LOCK.lock().unwrap();

        let store_pool_read = self.pool.read().unwrap();

        if let Some(store_kv) = store_pool_read.get(&pool_key) {
            Self::proceed_acquire_cache("kv", collection_str, pool_key, store_kv).map(Some)
        } else {
            info!(
                "kv store not in pool for collection: {} {}, opening it",
                collection_str, pool_key
            );

            drop(store_pool_read);

            let can_open_db = if mode == StoreKVAcquireMode::OpenOnly {
                self.kv_store_config.path(pool_key.collection_hash).exists()
            } else {
                true
            };

            let builder = StoreKVBuilder {
                kv_store_config: Arc::clone(&self.kv_store_config),
            };

            if can_open_db {
                Self::proceed_acquire_open("kv", collection_str, pool_key, &self.pool, &builder)
                    .map(Some)
            } else {
                Ok(None)
            }
        }
    }

    fn close(&self, collection_hash: StoreKVAtom) {
        debug!(
            "closing key-value database for collection: <{:x?}>",
            collection_hash
        );

        let mut store_pool_write = self.pool.write().unwrap();

        let collection_target = StoreKVKey::from_atom(collection_hash);

        store_pool_write.remove(&collection_target);
    }

    pub fn janitor(&self) {
        Self::proceed_janitor(
            "kv",
            &self.pool,
            self.kv_store_config.pool.inactive_after,
            &*STORE_ACCESS_LOCK,
        )
    }

    pub fn backup(&self, path: &Path) -> Result<(), io::Error> {
        debug!("backing up all kv stores to path: {:?}", path);

        fs::create_dir_all(path)?;

        self.dump_action(
            "backup",
            &self.kv_store_config.path,
            path,
            &Self::backup_item,
        )
    }

    pub fn restore(&self, path: &Path) -> Result<(), io::Error> {
        debug!("restoring all kv stores from path: {:?}", path);

        self.dump_action(
            "restore",
            path,
            &self.kv_store_config.path,
            &Self::restore_item,
        )
    }

    pub fn flush(&self, force: bool) {
        debug!("scanning for kv store pool items to flush to disk");

        let _flush = STORE_FLUSH_LOCK.lock().unwrap();

        let mut keys_flush: Vec<StoreKVKey> = Vec::new();

        {
            let _access = STORE_ACCESS_LOCK.write().unwrap();

            let store_pool_read = self.pool.read().unwrap();

            for (key, store) in &*store_pool_read {
                let not_flushed_for = store
                    .last_flushed
                    .read()
                    .unwrap()
                    .elapsed()
                    .unwrap_or_else(|err| {
                        error!(
                            "kv key: {} last flush duration clock issue, zeroing: {}",
                            key, err
                        );

                        Duration::from_secs(0)
                    })
                    .as_secs();

                if force || not_flushed_for >= self.kv_store_config.database.flush_after {
                    info!(
                        "kv key: {} not flushed for: {} seconds, may flush",
                        key, not_flushed_for
                    );

                    keys_flush.push(*key);
                } else {
                    debug!(
                        "kv key: {} not flushed for: {} seconds, no flush",
                        key, not_flushed_for
                    );
                }
            }
        }

        if keys_flush.is_empty() {
            info!("no kv store pool items need to be flushed at the moment");

            return;
        }

        let mut count_flushed = 0;

        {
            for key in &keys_flush {
                {
                    let _access = STORE_ACCESS_LOCK.write().unwrap();

                    if let Some(store) = self.pool.read().unwrap().get(key) {
                        debug!("kv key: {} flush started", key);

                        if let Err(err) = store.flush() {
                            error!("kv key: {} flush failed: {}", key, err);
                        } else {
                            count_flushed += 1;

                            debug!("kv key: {} flush complete", key);
                        }

                        *store.last_flushed.write().unwrap() = SystemTime::now();
                    }
                }

                thread::yield_now();
            }
        }

        info!(
            "done scanning for kv store pool items to flush to disk (flushed: {})",
            count_flushed
        );
    }

    #[allow(clippy::type_complexity)]
    fn dump_action(
        &self,
        action: &str,
        read_path: &Path,
        write_path: &Path,
        fn_item: &dyn Fn(&Self, &Path, &Path, &str) -> Result<(), io::Error>,
    ) -> Result<(), io::Error> {
        for collection in fs::read_dir(read_path)? {
            let collection = collection?;

            if let (Ok(collection_file_type), Some(collection_name)) =
                (collection.file_type(), collection.file_name().to_str())
            {
                if collection_file_type.is_dir() {
                    debug!("kv collection ongoing {}: {}", action, collection_name);

                    fn_item(self, write_path, &collection.path(), collection_name)?;
                }
            }
        }

        Ok(())
    }

    fn backup_item(
        &self,
        backup_path: &Path,
        _origin_path: &Path,
        collection_name: &str,
    ) -> Result<(), io::Error> {
        let _access = STORE_ACCESS_LOCK.write().unwrap();

        let kv_backup_path = backup_path.join(collection_name);

        debug!(
            "kv collection: {} backing up to path: {:?}",
            collection_name, kv_backup_path
        );

        if kv_backup_path.exists() {
            fs::remove_dir_all(&kv_backup_path)?;
        }

        fs::create_dir_all(backup_path.join(collection_name))?;

        if let Ok(collection_radix) = RadixNum::from_str(collection_name, ATOM_HASH_RADIX) {
            if let Ok(collection_hash) = collection_radix.as_decimal() {
                let origin_kv = StoreKVBuilder {
                    kv_store_config: Arc::clone(&self.kv_store_config),
                }
                .open(collection_hash as StoreKVAtom)
                .map_err(|_| io_error!("database open failure"))?;

                let kv_backup_options = DBBackupEngineOptions::new(&kv_backup_path)
                    .map_err(|_| io_error!("backup engine options acquire failure"))?;
                let kv_backup_environment = DBEnv::new()
                    .map_err(|_| io_error!("backup engine environment acquire failure"))?;

                let mut kv_backup_engine =
                    DBBackupEngine::open(&kv_backup_options, &kv_backup_environment)
                        .map_err(|_| io_error!("backup engine failure"))?;

                kv_backup_engine
                    .create_new_backup(&origin_kv)
                    .map_err(|_| io_error!("database backup failure"))?;

                info!(
                    "kv collection: {} backed up to path: {:?}",
                    collection_name, kv_backup_path
                );
            }
        }

        Ok(())
    }

    fn restore_item(
        &self,
        _backup_path: &Path,
        origin_path: &Path,
        collection_name: &str,
    ) -> Result<(), io::Error> {
        let _access = STORE_ACCESS_LOCK.write().unwrap();

        debug!(
            "kv collection: {} restoring from path: {:?}",
            collection_name, origin_path
        );

        if let Ok(collection_radix) = RadixNum::from_str(collection_name, ATOM_HASH_RADIX) {
            if let Ok(collection_hash) = collection_radix.as_decimal() {
                self.close(collection_hash as StoreKVAtom);

                let kv_path = self.kv_store_config.path(collection_hash as StoreKVAtom);

                if kv_path.exists() {
                    fs::remove_dir_all(&kv_path)?;
                }

                fs::create_dir_all(&kv_path)?;

                let kv_backup_options = DBBackupEngineOptions::new(&origin_path)
                    .map_err(|_| io_error!("backup engine options acquire failure"))?;
                let kv_backup_environment = DBEnv::new()
                    .map_err(|_| io_error!("backup engine environment acquire failure"))?;

                let mut kv_backup_engine =
                    DBBackupEngine::open(&kv_backup_options, &kv_backup_environment)
                        .map_err(|_| io_error!("backup engine failure"))?;

                kv_backup_engine
                    .restore_from_latest_backup(&kv_path, &kv_path, &DBRestoreOptions::default())
                    .map_err(|_| io_error!("database restore failure"))?;

                info!(
                    "kv collection: {} restored to path: {:?} from backup: {:?}",
                    collection_name, kv_path, origin_path
                );
            }
        }

        Ok(())
    }
}

impl StoreGenericPool<StoreKVKey, StoreKV, StoreKVBuilder> for StoreKVPool {}

impl StoreKVBuilder {
    fn open(&self, collection_hash: StoreKVAtom) -> Result<DB, DBError> {
        debug!(
            "opening key-value database for collection: <{:x?}>",
            collection_hash
        );

        let db_options = self.configure();

        DB::open(&db_options, self.kv_store_config.path(collection_hash))
    }

    fn configure(&self) -> DBOptions {
        debug!("configuring key-value database");

        let db_conf = &self.kv_store_config.database;

        let mut db_options = DBOptions::default();

        db_options.create_if_missing(true);
        db_options.set_use_fsync(false);
        db_options.set_compaction_style(DBCompactionStyle::Level);
        db_options.set_min_write_buffer_number(1);
        db_options.set_max_write_buffer_number(2);

        db_options.set_compression_type(if db_conf.compress {
            DBCompressionType::Zstd
        } else {
            DBCompressionType::None
        });

        db_options.set_max_open_files(if let Some(value) = db_conf.max_files {
            value as i32
        } else {
            -1
        });

        db_options.increase_parallelism(db_conf.parallelism as i32);
        db_options.set_max_subcompactions(db_conf.max_compactions as u32);
        db_options.set_max_background_jobs((db_conf.max_compactions + db_conf.max_flushes) as i32);
        db_options.set_write_buffer_size(db_conf.write_buffer * 1024);

        db_options
    }
}

impl crate::config::ConfigStoreKV {
    fn path(&self, collection_hash: StoreKVAtom) -> PathBuf {
        self.path.join(format!("{:x?}", collection_hash))
    }
}

impl StoreGenericBuilder<StoreKVKey, StoreKV> for StoreKVBuilder {
    fn build(&self, pool_key: StoreKVKey) -> Result<StoreKV, ()> {
        self.open(pool_key.collection_hash)
            .map(|db| {
                let now = SystemTime::now();

                StoreKV {
                    database: db,
                    collection_hash: pool_key.collection_hash,
                    last_used: Arc::new(RwLock::new(now)),
                    last_flushed: Arc::new(RwLock::new(now)),
                    lock: RwLock::new(false),
                    kv_store_config: Arc::clone(&self.kv_store_config),
                }
            })
            .map_err(|err| {
                error!("failed opening kv: {}", err);
            })
    }
}

impl StoreKV {
    pub fn get(&self, key: &[u8]) -> Result<Option<Vec<u8>>, DBError> {
        self.database.get(key)
    }

    pub fn put(&self, key: &[u8], data: &[u8]) -> Result<(), DBError> {
        let mut batch = WriteBatch::default();

        batch.put(key, data);

        self.do_write(batch)
    }

    pub fn delete(&self, key: &[u8]) -> Result<(), DBError> {
        let mut batch = WriteBatch::default();

        batch.delete(key);

        self.do_write(batch)
    }

    pub fn delete_prefix_range(&self, prefix: &[u8]) -> Result<(), DBError> {
        let start = prefix.to_vec();
        let mut end = prefix.to_vec();

        for byte in end.iter_mut().rev() {
            if *byte < 255 {
                *byte += 1;
                break;
            }

            *byte = 0;
        }

        let mut batch = WriteBatch::default();

        batch.delete_range(&start, &end);

        self.do_write(batch)
    }

    pub fn scan_prefix_values(
        &self,
        prefix: &[u8],
        value_budget: usize,
    ) -> Result<Vec<Vec<u8>>, DBError> {
        let iterator = self
            .database
            .iterator(IteratorMode::From(prefix, Direction::Forward));
        let mut values = Vec::new();

        for item in iterator {
            let (key, value) = item?;

            if !key.starts_with(prefix) {
                break;
            }

            values.push(value.to_vec());

            if values.len() >= value_budget {
                break;
            }
        }

        Ok(values)
    }

    fn flush(&self) -> Result<(), DBError> {
        let mut flush_options = FlushOptions::default();

        flush_options.set_wait(true);

        self.database.flush_opt(&flush_options)
    }

    fn do_write(&self, batch: WriteBatch) -> Result<(), DBError> {
        let mut write_options = WriteOptions::default();

        if !self.kv_store_config.database.write_ahead_log {
            debug!("ignoring wal for kv write");

            write_options.disable_wal(true);
        } else {
            debug!("using wal for kv write");

            write_options.disable_wal(false);
        }

        self.database.write_opt(batch, &write_options)
    }
}

impl StoreGeneric for StoreKV {
    fn ref_last_used(&self) -> &RwLock<SystemTime> {
        &self.last_used
    }
}

impl<'build> StoreKVActionBuilder<'build> {
    pub fn access(bucket: StoreItemPart, store: Option<StoreKVBox>) -> StoreKVAction {
        Self::build(bucket, store)
    }

    pub fn erase<'a, T: Into<&'a str>>(&self, collection: T, bucket: Option<T>) -> Result<u32, ()> {
        self.dispatch_erase("kv", collection, bucket)
    }

    fn build(bucket: StoreItemPart, store: Option<StoreKVBox>) -> StoreKVAction {
        StoreKVAction { store, bucket }
    }
}

impl<'build> StoreGenericActionBuilder for StoreKVActionBuilder<'build> {
    fn proceed_erase_collection(&self, collection_str: &str) -> Result<u32, ()> {
        let collection_atom = collection_str_to_hash(collection_str);
        let collection_path = self.kv_pool.kv_store_config.path(collection_atom);

        self.kv_pool.close(collection_atom);

        if collection_path.exists() {
            debug!(
                "kv collection store exists, erasing: {}/* at path: {:?}",
                collection_str, &collection_path
            );

            let erase_result = fs::remove_dir_all(&collection_path);

            if erase_result.is_ok() {
                debug!("done with kv collection erasure");

                Ok(1)
            } else {
                Err(())
            }
        } else {
            debug!(
                "kv collection store does not exist, consider already erased: {}/* at path: {:?}",
                collection_str, &collection_path
            );

            Ok(0)
        }
    }

    fn proceed_erase_bucket(&self, _collection: &str, _bucket: &str) -> Result<u32, ()> {
        // Bucket erasure requires acquiring the collection, which is handled inline
        // by the executor to avoid a party-killer dead-lock.
        Err(())
    }
}

impl<'a> StoreKVAction<'a> {
    pub fn get_pair_postings(&self, entry: PairEntry) -> Result<Option<Vec<(i32, u8)>>, ()> {
        if let Some(ref store) = self.store {
            let store_key = pair_key(
                store.collection_hash,
                bucket_str_to_hash(self.bucket.as_str()),
                entry,
            );

            match store.get(&store_key) {
                Ok(Some(value)) => decode_postings(&value).ok_or(()).map(Some),
                Ok(None) => Ok(None),
                Err(err) => {
                    error!("error getting direct pair posting with trace: {}", err);

                    Err(())
                }
            }
        } else {
            Ok(None)
        }
    }

    pub fn set_pair_postings(
        &self,
        entry: PairEntry,
        postings: &[(i32, u8)],
    ) -> Result<(), ()> {
        if let Some(ref store) = self.store {
            let store_key = pair_key(
                store.collection_hash,
                bucket_str_to_hash(self.bucket.as_str()),
                entry,
            );
            let encoded = encode_postings(postings);

            store.put(&store_key, &encoded).or(Err(()))
        } else {
            Err(())
        }
    }

    pub fn delete_pair_postings(&self, entry: PairEntry) -> Result<(), ()> {
        if let Some(ref store) = self.store {
            let store_key = pair_key(
                store.collection_hash,
                bucket_str_to_hash(self.bucket.as_str()),
                entry,
            );

            store.delete(&store_key).or(Err(()))
        } else {
            Err(())
        }
    }

    pub fn scan_pair_postings_by_first_word(
        &self,
        word: WordSlot,
        value_budget: usize,
    ) -> Result<Vec<(i32, u8)>, ()> {
        if let Some(ref store) = self.store {
            let collection_hash = store.collection_hash;
            let bucket_hash = bucket_str_to_hash(self.bucket.as_str());
            let mut prefixes = Vec::new();

            match word {
                WordSlot::Short(word1_sig32) => {
                    prefixes.push(compact_pair_first_word_prefix(
                        collection_hash,
                        bucket_hash,
                        word1_sig32,
                    ));
                    prefixes.push(extended_pair_first_word_prefix(
                        collection_hash,
                        bucket_hash,
                        word,
                    ));
                }
                WordSlot::Long(_) => prefixes.push(extended_pair_first_word_prefix(
                    collection_hash,
                    bucket_hash,
                    word,
                )),
            }

            self.scan_postings_for_prefixes(prefixes, value_budget)
        } else {
            Ok(Vec::new())
        }
    }

    pub fn scan_pair_postings_by_first_word_units(
        &self,
        units: &[u8],
        value_budget: usize,
    ) -> Result<Vec<(i32, u8)>, ()> {
        if let Some(ref store) = self.store {
            let collection_hash = store.collection_hash;
            let bucket_hash = bucket_str_to_hash(self.bucket.as_str());
            let mut prefixes = Vec::new();

            if units.len() <= 4 {
                prefixes.push(compact_pair_first_word_unit_prefix(
                    collection_hash,
                    bucket_hash,
                    units,
                ));
                prefixes.push(extended_pair_first_word_unit_prefix(
                    collection_hash,
                    bucket_hash,
                    1,
                    units,
                ));
            }

            prefixes.push(extended_pair_first_word_unit_prefix(
                collection_hash,
                bucket_hash,
                2,
                units,
            ));

            self.scan_postings_for_prefixes(prefixes, value_budget)
        } else {
            Ok(Vec::new())
        }
    }

    pub fn scan_pair_postings_by_second_word_units(
        &self,
        first_word: WordSlot,
        second_units: &[u8],
        value_budget: usize,
    ) -> Result<Vec<(i32, u8)>, ()> {
        if let Some(ref store) = self.store {
            let collection_hash = store.collection_hash;
            let bucket_hash = bucket_str_to_hash(self.bucket.as_str());
            let mut prefixes = Vec::new();

            if let WordSlot::Short(first_sig32) = first_word
                && second_units.len() <= 4
            {
                prefixes.push(compact_pair_second_word_unit_prefix(
                    collection_hash,
                    bucket_hash,
                    first_sig32,
                    second_units,
                ));
            }

            if second_units.len() <= 4 {
                prefixes.push(extended_pair_second_word_unit_prefix(
                    collection_hash,
                    bucket_hash,
                    first_word,
                    1,
                    second_units,
                ));
            }

            prefixes.push(extended_pair_second_word_unit_prefix(
                collection_hash,
                bucket_hash,
                first_word,
                2,
                second_units,
            ));

            self.scan_postings_for_prefixes(prefixes, value_budget)
        } else {
            Ok(Vec::new())
        }
    }

    pub fn get_doc_reverse_refs(&self, doc_id: i32) -> Result<Option<Vec<PairSignatureRef>>, ()> {
        if let Some(ref store) = self.store {
            let store_key = doc_reverse_key(
                store.collection_hash,
                bucket_str_to_hash(self.bucket.as_str()),
                doc_id,
            );

            match store.get(&store_key) {
                Ok(Some(value)) => decode_reverse_refs(&value).ok_or(()).map(Some),
                Ok(None) => Ok(None),
                Err(err) => {
                    error!("error getting direct doc reverse refs with trace: {}", err);

                    Err(())
                }
            }
        } else {
            Ok(None)
        }
    }

    pub fn set_doc_reverse_refs(&self, doc_id: i32, refs: &[PairSignatureRef]) -> Result<(), ()> {
        if let Some(ref store) = self.store {
            let store_key = doc_reverse_key(
                store.collection_hash,
                bucket_str_to_hash(self.bucket.as_str()),
                doc_id,
            );
            let refs_encoded = encode_reverse_refs(refs);

            store.put(&store_key, &refs_encoded).or(Err(()))
        } else {
            Err(())
        }
    }

    pub fn delete_doc_reverse_refs(&self, doc_id: i32) -> Result<(), ()> {
        if let Some(ref store) = self.store {
            let store_key = doc_reverse_key(
                store.collection_hash,
                bucket_str_to_hash(self.bucket.as_str()),
                doc_id,
            );

            store.delete(&store_key).or(Err(()))
        } else {
            Err(())
        }
    }

    pub fn insert_pair_posting(
        &self,
        entry: PairEntry,
        doc_id: i32,
        meta: PostingMeta,
    ) -> Result<(), ()> {
        let mut postings = self.get_pair_postings(entry)?.unwrap_or_default();
        let meta_byte = meta.pack();

        if !postings
            .iter()
            .any(|(existing_id, existing_meta)| *existing_id == doc_id && *existing_meta == meta_byte)
        {
            postings.push((doc_id, meta_byte));
            postings.sort_by_key(|(doc_id, _)| sortable_doc_id(*doc_id));
            self.set_pair_postings(entry, &postings)?;
        }

        Ok(())
    }

    pub fn remove_pair_postings_for_doc(&self, entry: PairEntry, doc_id: i32) -> Result<bool, ()> {
        let Some(mut postings) = self.get_pair_postings(entry)? else {
            return Ok(false);
        };

        let original_len = postings.len();

        postings.retain(|(current_doc_id, _)| *current_doc_id != doc_id);

        if postings.len() == original_len {
            return Ok(false);
        }

        if postings.is_empty() {
            self.delete_pair_postings(entry)?;
        } else {
            self.set_pair_postings(entry, &postings)?;
        }

        Ok(true)
    }

    pub fn replace_direct_pair_document(
        &self,
        doc_id: i32,
        entries: &[(PairEntry, PostingMeta)],
    ) -> Result<(), ()> {
        self.delete_direct_pair_document(doc_id)?;

        for (entry, meta) in entries {
            self.insert_pair_posting(*entry, doc_id, *meta)?;
        }

        let refs: Vec<PairSignatureRef> = entries
            .iter()
            .map(|(entry, meta)| pair_signature_ref(*entry, meta.kind))
            .collect();

        self.set_doc_reverse_refs(doc_id, &refs)
    }

    pub fn delete_direct_pair_document(&self, doc_id: i32) -> Result<u32, ()> {
        let Some(refs) = self.get_doc_reverse_refs(doc_id)? else {
            return Ok(0);
        };

        let mut removed = 0;

        for signature_ref in &refs {
            if self.remove_pair_postings_for_doc((*signature_ref).into(), doc_id)? {
                removed += 1;
            }
        }

        self.delete_doc_reverse_refs(doc_id)?;

        Ok(removed)
    }

    fn scan_postings_for_prefixes(
        &self,
        prefixes: Vec<Vec<u8>>,
        value_budget: usize,
    ) -> Result<Vec<(i32, u8)>, ()> {
        let Some(ref store) = self.store else {
            return Ok(Vec::new());
        };
        let posting_budget = value_budget.saturating_mul(POSTING_WIDTH).max(POSTING_WIDTH);
        let mut postings = Vec::new();

        for prefix in prefixes {
            let values = store
                .scan_prefix_values(&prefix, posting_budget)
                .map_err(|err| {
                    error!("error scanning direct pair prefix with trace: {}", err);
                })?;

            for value in values {
                if let Some(mut value_postings) = decode_postings(&value) {
                    postings.append(&mut value_postings);
                } else {
                    return Err(());
                }

                if postings.len() >= value_budget {
                    postings.truncate(value_budget);

                    return Ok(postings);
                }
            }
        }

        Ok(postings)
    }

    pub fn batch_erase_bucket(&self) -> Result<u32, ()> {
        if let Some(ref store) = self.store {
            let bucket_hash = bucket_str_to_hash(self.bucket.as_str());
            let prefixes = [
                compact_pair_bucket_prefix(store.collection_hash, bucket_hash),
                extended_pair_bucket_prefix(store.collection_hash, bucket_hash),
                doc_reverse_bucket_prefix(store.collection_hash, bucket_hash),
            ];

            for key_prefix in &prefixes {
                if let Err(err) = store.delete_prefix_range(key_prefix) {
                    error!(
                        "failed erasing direct pair bucket prefix: {:?} with error: {}",
                        key_prefix, err
                    );
                }
            }

            info!(
                "done processing store batch erase bucket: {}",
                self.bucket.as_str()
            );

            Ok(1)
        } else {
            Err(())
        }
    }
}

impl From<PairSignatureRef> for PairEntry {
    fn from(signature_ref: PairSignatureRef) -> Self {
        PairEntry {
            word1: signature_ref.word1,
            word2: signature_ref.word2,
        }
    }
}

impl StoreKVKey {
    pub fn from_atom(collection_hash: StoreKVAtom) -> StoreKVKey {
        StoreKVKey { collection_hash }
    }

    #[allow(clippy::should_implement_trait)]
    pub fn from_str(collection_str: &str) -> StoreKVKey {
        StoreKVKey {
            collection_hash: collection_str_to_hash(collection_str),
        }
    }
}

impl fmt::Display for StoreKVKey {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "<{:x?}>", self.collection_hash)
    }
}

fn collection_str_to_hash(value: &str) -> StoreKVAtom {
    fnv_hash_32(value.as_bytes())
}

fn bucket_str_to_hash(value: &str) -> u32 {
    fnv_hash_32(value.as_bytes())
}

fn fnv_hash_32(bytes: &[u8]) -> u32 {
    let mut hash: u32 = 0x811c9dc5;

    for byte in bytes {
        hash ^= u32::from(*byte);
        hash = hash.wrapping_mul(0x01000193);
    }

    hash
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pair::{PairKind, PostingMeta, encode_text, pair_entries};

    #[test]
    fn it_acquires_database() {
        let kv_store_config = test_kv_store_config();
        let kv_pool = StoreKVPool::new(kv_store_config);

        assert!(kv_pool.acquire(StoreKVAcquireMode::Any, "c:test:1").is_ok());
    }

    #[test]
    fn it_janitors_database() {
        let kv_store_config = test_kv_store_config();
        let kv_pool = StoreKVPool::new(kv_store_config);

        kv_pool.janitor();
    }

    #[test]
    fn it_proceeds_primitives() {
        let kv_store_config = test_kv_store_config();
        let kv_pool = StoreKVPool::new(kv_store_config);

        let store = kv_pool
            .acquire(StoreKVAcquireMode::Any, "c:test:2")
            .unwrap()
            .unwrap();

        assert!(store.get(&[0]).is_ok());
        assert!(store.put(&[0], &[1, 0, 0, 0]).is_ok());
        assert!(store.delete(&[0]).is_ok());
    }

    #[test]
    fn it_proceeds_direct_pair_actions() {
        let kv_store_config = test_kv_store_config();
        let kv_pool = StoreKVPool::new(kv_store_config);

        let store = kv_pool
            .acquire(StoreKVAcquireMode::Any, "c:test:direct-pair-actions")
            .unwrap();
        let action = StoreKVActionBuilder::access(
            StoreItemPart::from_str("b:test:direct-pair-actions").unwrap(),
            store,
        );

        let entries = pair_entries(&encode_text("tequila sunrise"));

        assert_eq!(entries.len(), 2);
        assert_eq!(entries[0].1.kind, PairKind::Adjacent);
        assert_eq!(entries[1].1.kind, PairKind::Wrap);
        assert!(action.replace_direct_pair_document(-7, &entries).is_ok());

        let adjacent_postings = action.get_pair_postings(entries[0].0).unwrap().unwrap();
        assert_eq!(adjacent_postings.len(), 1);
        assert_eq!(adjacent_postings[0].0, -7);
        assert_eq!(
            PostingMeta::unpack(adjacent_postings[0].1).kind,
            PairKind::Adjacent
        );

        assert_eq!(
            action.get_doc_reverse_refs(-7).unwrap().unwrap().len(),
            entries.len()
        );

        assert_eq!(action.delete_direct_pair_document(-7), Ok(2));
        assert_eq!(action.get_pair_postings(entries[0].0).unwrap(), None);
        assert_eq!(action.get_doc_reverse_refs(-7).unwrap(), None);
    }

    #[test]
    fn it_replaces_direct_pair_documents() {
        let kv_store_config = test_kv_store_config();
        let kv_pool = StoreKVPool::new(kv_store_config);

        let store = kv_pool
            .acquire(StoreKVAcquireMode::Any, "c:test:direct-pair-replace")
            .unwrap();
        let action = StoreKVActionBuilder::access(
            StoreItemPart::from_str("b:test:direct-pair-replace").unwrap(),
            store,
        );

        let old_entries = pair_entries(&encode_text("agua mineral"));
        let new_entries = pair_entries(&encode_text("agua gas"));

        assert!(
            action
                .replace_direct_pair_document(42, &old_entries)
                .is_ok()
        );
        assert!(
            action
                .replace_direct_pair_document(42, &new_entries)
                .is_ok()
        );

        assert_eq!(action.get_pair_postings(old_entries[0].0).unwrap(), None);

        let new_postings = action.get_pair_postings(new_entries[0].0).unwrap().unwrap();
        assert_eq!(new_postings.len(), 1);
        assert_eq!(new_postings[0].0, 42);
    }

    fn test_kv_store_config() -> Arc<crate::config::ConfigStoreKV> {
        Arc::new(
            config::Config::builder()
                .add_source(config::File::from_str(
                    crate::config::tests::defaults_toml(),
                    config::FileFormat::Toml,
                ))
                .build()
                .unwrap()
                .get::<crate::config::ConfigStoreKV>("store.kv")
                .unwrap(),
        )
    }
}
