# Build the binaries in larger image
FROM docker.io/fortio/fortio.build:v79@sha256:614de3fd5430a3502ab408f35a293e5cf1364695b6cc10a5168507ed519f55c2 AS build
WORKDIR /build
COPY --chown=build:build . fortio
ARG MODE=install
# We moved a lot of the logic into the Makefile so it can be reused in brew
# but that also couples the 2, this expects to find binaries in the right place etc
RUN make -C fortio official-build-version BUILD_DIR=/build MODE=${MODE}

# Minimal image with just the binary and certs
FROM scratch AS release
# We don't need to copy certs anymore since cli 1.6.0
# COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /build/result/fortio /usr/bin/fortio
EXPOSE 8078
EXPOSE 8079
EXPOSE 8080
EXPOSE 8081
# configmap (dynamic flags)
VOLUME /etc/fortio
# data files etc
VOLUME /var/lib/fortio
WORKDIR /var/lib/fortio
ENTRYPOINT ["/usr/bin/fortio"]
# start the server mode (grpc ping on 8079, http echo and UI on 8080, redirector on 8081) by default
CMD ["server", "-config-dir", "/etc/fortio"]
