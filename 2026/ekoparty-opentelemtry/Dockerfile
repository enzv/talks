FROM golang:1.26.1-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -o /out/trace-lab .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/trace-lab /app/trace-lab

USER nonroot:nonroot
ENTRYPOINT ["/app/trace-lab"]
