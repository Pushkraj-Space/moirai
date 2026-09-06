FROM golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/moirai-cloud ./cmd/moirai-cloud
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/moirai-cloud /moirai-cloud
ENV MOIRAI_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/moirai-cloud"]
