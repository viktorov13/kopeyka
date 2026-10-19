FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/expenses ./cmd/server

FROM build AS test
RUN go vet ./...
CMD ["go", "test", "-count=1", "./..."]

FROM alpine:3.23

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 expenses \
    && adduser -S -D -H -u 10001 -G expenses expenses
WORKDIR /app
COPY --from=build /out/expenses /app/expenses
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/expenses"]
