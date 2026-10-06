// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use std::net::TcpListener;
use std::process;
use std::sync::{Arc, RwLock};
use std::thread;

use genixsearch::store::kv::StoreKVPool;

use super::handle::ChannelHandle;
use crate::THREAD_NAME_CHANNEL_CLIENT;

#[derive(Clone)]
pub struct ChannelListenBuilder {
    pub app_conf: Arc<genixsearch::Config>,
    pub kv_pool: StoreKVPool,
}

pub struct ChannelListen {
    app_conf: Arc<genixsearch::Config>,
    kv_pool: StoreKVPool,
}

lazy_static! {
    pub static ref CHANNEL_AVAILABLE: RwLock<bool> = RwLock::new(true);
}

impl ChannelListenBuilder {
    pub fn build(&self) -> ChannelListen {
        ChannelListen {
            app_conf: Arc::clone(&self.app_conf),
            kv_pool: self.kv_pool.clone(),
        }
    }
}

impl ChannelListen {
    pub fn run(&self) {
        match TcpListener::bind(self.app_conf.channel.inet) {
            Ok(listener) => {
                info!("listening on tcp://{}", self.app_conf.channel.inet);

                for stream in listener.incoming() {
                    match stream {
                        Ok(stream) => {
                            let handle = ChannelHandle {
                                channel_config: Arc::clone(&self.app_conf.channel),
                                executor: genixsearch::Executor {
                                    app_conf: Arc::clone(&self.app_conf),
                                    kv_pool: self.kv_pool.clone(),
                                },
                            };

                            thread::Builder::new()
                                .name(THREAD_NAME_CHANNEL_CLIENT.to_string())
                                .spawn(move || {
                                    if let Ok(peer_addr) = stream.peer_addr() {
                                        debug!("channel client connecting: {}", peer_addr);
                                    }

                                    handle.client(stream);
                                })
                                .ok();
                        }
                        Err(err) => {
                            warn!("error handling stream: {}", err);
                        }
                    }
                }
            }
            Err(err) => {
                error!("error binding channel listener: {}", err);

                process::exit(1);
            }
        }
    }

    pub fn teardown() {
        *CHANNEL_AVAILABLE.write().unwrap() = false;
    }
}
