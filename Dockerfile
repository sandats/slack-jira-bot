# syntax=docker/dockerfile:1
FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/slack-jira-bot ./cmd/slack-jira-bot

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/slack-jira-bot /slack-jira-bot
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/slack-jira-bot"]
