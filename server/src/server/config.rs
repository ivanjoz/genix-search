// GenixSearch
//
// Fast, lightweight and schema-less search backend
// Copyright: 2019, Valerian Saliou <valerian@valeriansaliou.name>
// Copyright: 2026, Rémi Bardon <remi@remibardon.name>
// License: Mozilla Public License v2.0 (MPL v2.0)

pub fn defaults_toml() -> &'static str {
    r#"
    [server]
    log_level = "error"

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

pub fn read_config(app_args: &crate::AppArgs) -> Config {
    let config_path = &app_args.config;

    if config_path != crate::DEFAULT_CONFIG_FILE_PATH && !std::path::Path::new(config_path).exists()
    {
        panic!("Cannot find config file at '{config_path}'");
    }

    debug!("reading config file: {config_path}");

    let raw_config: config::Config = config::Config::builder()
        .add_source(config::File::from_str(
            defaults_toml(),
            config::FileFormat::Toml,
        ))
        .add_source(config::File::new(config_path, config::FileFormat::Toml).required(false))
        .add_source(
            config::Environment::with_prefix("GENIXSEARCH")
                .separator("__")
                .prefix_separator("_"),
        )
        .build()
        .expect("error reading config");

    let config = raw_config
        .try_deserialize::<Config>()
        .expect("syntax error in config");

    config.genixsearch.validate();

    config
}

#[derive(serde::Deserialize)]
pub struct Config {
    pub server: ConfigServer,

    #[serde(flatten)]
    pub genixsearch: genixsearch::Config,
}

#[derive(serde::Deserialize)]
pub struct ConfigServer {
    #[serde(deserialize_with = "genixsearch::util::serde::env_var::str")]
    pub log_level: String,
}
