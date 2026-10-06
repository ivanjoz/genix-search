FROM rust:slim-bullseye AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential clang \
    && rm -rf /var/lib/apt/lists/*

RUN rustc --version && cargo --version && rustup --version

WORKDIR /app
COPY . /app

RUN cargo build --locked --release --bin genixsearch
RUN strip ./target/release/genixsearch

FROM gcr.io/distroless/cc

WORKDIR /usr/src/genixsearch

COPY --from=build /app/target/release/genixsearch /usr/local/bin/genixsearch

CMD [ "genixsearch", "-c", "/etc/genixsearch.cfg" ]

EXPOSE 1491
