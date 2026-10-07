# The control plane image Cloud Build makes: the static binary and nothing
# else. It runs as a non-root user number so the Kubernetes manifest can
# require it.
FROM scratch
COPY out/benchgrid out/bgctl /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/benchgrid"]
