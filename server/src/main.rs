// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

#![deny(
    clippy::all,
    dead_code,
    unstable_features,
    unused_imports,
    unused_qualifications
)]
#![warn(
    clippy::inline_always, // Do not use unless benchmarked (explicit allow).
)]
#![allow(
    clippy::collapsible_if, // Style preference.
    clippy::explicit_auto_deref, // Style preference.
    clippy::needless_as_bytes, // Style preference. Better make those things explicit.
    clippy::needless_borrow, // Style preference.
    clippy::needless_borrows_for_generic_args, // Style preference.
)]

#[macro_use]
extern crate log;
#[macro_use]
extern crate lazy_static;

mod channel;
mod server;
mod tasker;

use std::ops::Deref;
use std::str::FromStr;
use std::sync::Arc;
use std::thread;
use std::time::Duration;

use clap::{Arg, Command};
use log::LevelFilter;

use channel::listen::{ChannelListen, ChannelListenBuilder};
use channel::statistics::ensure_states as ensure_states_channel_statistics;
use genixsearch::store::kv::StoreKVPool;
use tasker::runtime::TaskerBuilder;
use tasker::shutdown::ShutdownSignal;

use crate::server::config::read_config;
use crate::server::logger::ConfigLogger;

struct AppArgs {
    config: String,
}

#[cfg(unix)]
#[cfg(feature = "allocator-jemalloc")]
#[global_allocator]
static ALLOC: tikv_jemallocator::Jemalloc = tikv_jemallocator::Jemalloc;

pub static LINE_FEED: &str = "\r\n";

pub static THREAD_NAME_CHANNEL_MASTER: &str = "genixsearch-channel-master";
pub static THREAD_NAME_CHANNEL_CLIENT: &str = "genixsearch-channel-client";
pub static THREAD_NAME_TASKER: &str = "genixsearch-tasker";

lazy_static! {
    static ref APP_ARGS: AppArgs = make_app_args();
}

const DEFAULT_CONFIG_FILE_PATH: &str = "./genixsearch.cfg";

fn make_app_args() -> AppArgs {
    let matches = Command::new(clap::crate_name!())
        .version(clap::crate_version!())
        .author(clap::crate_authors!())
        .about(clap::crate_description!())
        .arg(
            Arg::new("config")
                .short('c')
                .long("config")
                .help("Path to configuration file")
                .default_value(DEFAULT_CONFIG_FILE_PATH),
        )
        .get_matches();

    AppArgs {
        config: matches
            .get_one::<String>("config")
            .expect("invalid config value")
            .to_owned(),
    }
}

fn main() {
    let app_conf = read_config(&APP_ARGS);

    let _logger = ConfigLogger::init(
        LevelFilter::from_str(&app_conf.server.log_level).expect("invalid log level"),
    );

    let shutdown_signal = ShutdownSignal::new();

    info!("starting up");

    ensure_states();

    let kv_pool = StoreKVPool::new(Arc::clone(&app_conf.genixsearch.store.kv));

    thread::spawn(spawn_tasker(kv_pool.clone()));

    thread::spawn(spawn_channel(kv_pool.clone(), app_conf.genixsearch.into()));

    info!("started");

    shutdown_signal.at_exit(move |signal| {
        info!("stopping gracefully (got signal: {})", signal);

        ChannelListen::teardown();

        kv_pool.flush(true);

        info!("stopped");
    });
}

fn ensure_states() {
    let _ = APP_ARGS.deref();

    ensure_states_channel_statistics();
}

fn spawn_managed_thread<F, T>(name: &'static str, thread_name: &'static str, task: F)
where
    F: Fn() -> T + Clone,
    F: Send + 'static,
    T: Send + 'static,
{
    debug!("spawn managed thread: {name}");

    let worker = thread::Builder::new()
        .name(thread_name.to_string())
        .spawn(task.clone());

    let has_error = if let Ok(worker_thread) = worker {
        worker_thread.join().is_err()
    } else {
        true
    };

    if has_error {
        error!("managed thread crashed ({name}), setting it up again");

        thread::sleep(Duration::from_secs(1));

        spawn_managed_thread(name, thread_name, task);
    }
}

fn spawn_channel(kv_pool: StoreKVPool, app_conf: Arc<genixsearch::Config>) -> impl FnOnce() {
    let builder = ChannelListenBuilder { app_conf, kv_pool };

    move || {
        spawn_managed_thread("channel", THREAD_NAME_CHANNEL_MASTER, move || {
            builder.build().run();
        })
    }
}

fn spawn_tasker(kv_pool: StoreKVPool) -> impl FnOnce() {
    let builder = TaskerBuilder { kv_pool };

    move || {
        spawn_managed_thread("tasker", THREAD_NAME_TASKER, move || {
            builder.build().run();
        })
    }
}
