// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

//! GenixSearch library configuration.
//!
//! It does not include server nor channel configuration, which are specific
//! to the `genixsearch` binary.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use serde::Deserialize;

use crate::util::serde::env_var;

#[derive(Deserialize)]
pub struct Config {
    pub channel: Arc<ConfigChannel>,

    pub store: ConfigStore,
}

impl Config {
    pub fn validate(&self) {
        if self.store.kv.database.write_buffer == 0 {
            panic!("write_buffer for kv must not be zero");
        }

        if self.store.kv.database.flush_after >= self.store.kv.pool.inactive_after {
            panic!("flush_after for kv must be strictly lower than inactive_after");
        }
    }
}

#[derive(Deserialize)]
pub struct ConfigChannel {
    #[serde(deserialize_with = "env_var::socket_addr")]
    pub inet: SocketAddr,

    pub tcp_timeout: u64,

    #[serde(default, deserialize_with = "env_var::opt_str")]
    pub auth_password: Option<String>,

    pub search: ConfigChannelSearch,
}

#[derive(Deserialize)]
pub struct ConfigChannelSearch {
    pub query_limit_default: u16,

    pub query_limit_maximum: u16,
}

#[derive(Deserialize)]
pub struct ConfigStore {
    pub kv: Arc<ConfigStoreKV>,
}

#[derive(Deserialize)]
pub struct ConfigStoreKV {
    #[serde(deserialize_with = "env_var::path_buf")]
    pub path: PathBuf,

    pub pool: ConfigStoreKVPool,

    pub database: ConfigStoreKVDatabase,
}

#[derive(Deserialize)]
pub struct ConfigStoreKVPool {
    pub inactive_after: u64,
}

#[derive(Deserialize)]
pub struct ConfigStoreKVDatabase {
    pub flush_after: u64,

    pub compress: bool,

    pub parallelism: u16,

    pub max_files: Option<u32>,

    pub max_compactions: u16,

    pub max_flushes: u16,

    pub write_buffer: usize,

    pub write_ahead_log: bool,
}

#[cfg(test)]
pub(crate) mod tests {
    pub fn defaults_toml() -> &'static str {
        r#"
        [channel]
        inet = "[::1]:1491"
        tcp_timeout = 300
        search.query_limit_default = 10
        search.query_limit_maximum = 100

        [store.kv]
        path = "./data/store/kv/"
        pool.inactive_after = 1800
        database.flush_after = 900
        database.compress = true
        database.parallelism = 2
        database.max_compactions = 1
        database.max_flushes = 1
        database.write_buffer = 16384
        database.write_ahead_log = true
        "#
    }
}
