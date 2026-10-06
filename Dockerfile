FROM gcr.io/distroless/static-debian12
COPY seedarr-linux-amd64 /seedarr
ENTRYPOINT ["/seedarr"]
