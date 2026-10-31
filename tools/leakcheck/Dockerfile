# Isolated exiftool + mat2 environment used by the leak-checker agent.
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends libimage-exiftool-perl mat2 \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home checker
USER checker
WORKDIR /files
