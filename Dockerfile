# LawPay and DocuSeal module image. The entrypoint serves the module; the same binary
# packages it with `lawpay-docuseal package`.
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/lawpay-docuseal ./cmd/lawpay-docuseal

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lawpay-docuseal /lawpay-docuseal
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/lawpay-docuseal"]
CMD ["serve"]
