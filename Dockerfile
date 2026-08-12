# SPDX-License-Identifier: GPL-3.0-or-later
#
# Build: docker build -t stattii:local --build-arg VERSION="$(git describe --tags --always --dirty)" .
#
# Run stage is distroless/static (CA bundle for SMTP TLS, tzdata, nonroot);
# the binary additionally embeds tzdata (-tags timetzdata) so TZ=Europe/Berlin
# keeps working even if the base image ever drops zoneinfo.

FROM golang:1.26 AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -tags timetzdata \
      -ldflags "-s -w -X main.version=${VERSION}" -o /out/stattii .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/stattii /stattii
EXPOSE 8788
ENTRYPOINT ["/stattii"]
CMD ["serve"]
