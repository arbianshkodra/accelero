FROM --platform=$BUILDPLATFORM alpine:3.22

ARG TARGETARCH
ARG TARGETVARIANT

RUN apk add --no-cache ca-certificates curl

EXPOSE 8000

VOLUME /data

ENV DATABASE_PATH=/data/accelero.db

COPY dist/accelero_linux_${TARGETARCH} /accelero

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -f http://localhost:8000/health || exit 1

ENTRYPOINT ["/accelero"]
