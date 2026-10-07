# One image for every benchgrid process. It holds statically linked binaries
# and nothing else: no shell, no package manager, nothing to patch. Build the
# binaries first with `make linux`; building inside the image would need a Go
# toolchain image, and this repository's evidence was produced without
# pulling anything from a registry.
FROM scratch
COPY bin/linux/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/benchgrid"]
