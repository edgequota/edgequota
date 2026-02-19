FROM golang:1.26 AS builder
WORKDIR /app
RUN go mod init mockextrl
COPY main.go .
RUN CGO_ENABLED=0 go build -o /mockextrl .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /mockextrl /mockextrl
ENTRYPOINT ["/mockextrl"]
