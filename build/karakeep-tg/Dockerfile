FROM gcr.io/distroless/static:nonroot
ARG TARGETARCH
COPY karakeep-tg-${TARGETARCH} /karakeep-tg
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 CMD ["/karakeep-tg", "healthcheck"]
ENTRYPOINT ["/karakeep-tg"]
