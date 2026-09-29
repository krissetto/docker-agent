# Reuse the published v3 application unchanged; v2 retains the image entrypoint for its idle keeper.
FROM christopherpetito053/docker-agent@sha256:b2f47711c41597764baeafe8e53ebbd688a60aa4ca5946a64070402755984b49
ENTRYPOINT ["/usr/bin/tini","--"]
CMD []
