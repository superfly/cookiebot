FROM golang:latest

RUN mkdir /app
WORKDIR /app
ADD go.mod /app/
ADD go.sum /app/
ADD *.go /app/
RUN go build
CMD ./cookiebot

