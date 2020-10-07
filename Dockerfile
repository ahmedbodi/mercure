# Compile stage
FROM golang:1.13.8 AS build-env
ADD . /dockerdev
WORKDIR /dockerdev
RUN go build -o /server

# Final stage
FROM debian:buster
EXPOSE 80 443 3000
WORKDIR /
COPY --from=build-env /server /
CMD ["/server"]