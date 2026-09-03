FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/causely-background-agent .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && \
    addgroup -S causely -g 3000 && \
    adduser -S -D -H -h /home/causely -s /bin/sh -u 2000 -G causely causely
COPY --from=build /out/causely-background-agent /bin/causely-background-agent
USER causely
ENTRYPOINT ["/bin/causely-background-agent"]
