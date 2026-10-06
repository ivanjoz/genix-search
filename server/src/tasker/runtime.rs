// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

use std::thread;
use std::time::{Duration, Instant};

use genixsearch::store::kv::StoreKVPool;

#[derive(Clone)]
pub struct TaskerBuilder {
    pub kv_pool: StoreKVPool,
}

pub struct Tasker {
    kv_pool: StoreKVPool,
}

const TASKER_TICK_INTERVAL: Duration = Duration::from_secs(10);

impl TaskerBuilder {
    pub fn build(&self) -> Tasker {
        Tasker {
            kv_pool: self.kv_pool.clone(),
        }
    }
}

impl Tasker {
    pub fn run(&self) {
        info!("tasker is now active");

        loop {
            thread::sleep(TASKER_TICK_INTERVAL);

            debug!("running a tasker tick...");

            let tick_start = Instant::now();

            self.tick();

            let tick_took = tick_start.elapsed();

            info!(
                "ran tasker tick (took {}s + {}ms)",
                tick_took.as_secs(),
                tick_took.subsec_millis()
            );
        }
    }

    fn tick(&self) {
        self.kv_pool.janitor();
        self.kv_pool.flush(false);
    }
}
