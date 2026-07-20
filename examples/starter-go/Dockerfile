# Copyright 2026. Licensed under the Apache License, Version 2.0.
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /agent .

FROM gcr.io/distroless/static:nonroot
COPY --from=build /agent /agent
ENTRYPOINT ["/agent"]
