FROM 10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify@sha256:c98767e78dfd6babe2527b7eac4068a4592d0e93b80514bffd343293ae2aea55
COPY --chmod=0555 readonly-check /readonly-check
USER 107:107
ENTRYPOINT ["/readonly-check"]
