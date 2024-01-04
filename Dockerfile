FROM golang:latest

WORKDIR /app
ADD go.mod /app/
ADD go.sum /app/
ADD *.go /app/
ADD cookiebot.yml /app/
RUN go build
CMD ./cookiebot

