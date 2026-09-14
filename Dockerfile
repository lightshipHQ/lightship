FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /lightship ./cmd/lightship

FROM gcr.io/distroless/static-debian12
COPY --from=build /lightship /lightship
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/lightship"]
CMD ["serve"]
