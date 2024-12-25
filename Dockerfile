FROM alpine:latest

ARG BUILD_DATE
ARG VERSION

LABEL org.opencontainers.image.created=$BUILD_DATE
LABEL org.opencontainers.image.version=$VERSION
LABEL org.opencontainers.image.source="https://github.com/lumeweb/promster"

COPY ./promster /bin/promster

ENTRYPOINT ["/bin/promster"]
