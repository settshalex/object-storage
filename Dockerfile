FROM golang:1.22.5  AS build
WORKDIR /mnt/homework
COPY . .
RUN go build -o object-storage .

# Docker is used as a base image so you can easily start playing around in the container using the Docker command line client.
FROM gcr.io/distroless/cc-debian12:debug-nonroot
COPY --from=build /mnt/homework/object-storage /home/project/
WORKDIR /home/project

ENTRYPOINT ["./object-storage"]