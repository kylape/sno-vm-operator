FROM scratch
LABEL org.opencontainers.image.source="https://github.com/kylape/sno-vm-operator"
COPY --chmod=0555 bin/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
