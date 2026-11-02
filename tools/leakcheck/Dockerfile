# exiftool and mat2 in a container, for cross-checking stripped files:
#   docker build -t traceless-leakcheck tools/leakcheck
#   docker run --rm -v "$PWD":/files:ro traceless-leakcheck exiftool -a -G1 -s photo.clean.png
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends libimage-exiftool-perl mat2 \
 && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home checker
USER checker
WORKDIR /files
