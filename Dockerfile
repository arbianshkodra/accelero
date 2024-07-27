FROM golang:1.22.5-alpine3.20 as builder

# Set the Current Working Directory inside the container
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies. Dependencies will be cached if the go.mod and go.sum files are not changed
RUN go mod download

# Copy the source from the current directory to the Working Directory inside the container
COPY . .

# Build the Go app
RUN CGO_ENABLED=0 GOOS=linux go build -o cmd/main cmd/main.go

# Start a new stage from scratch
FROM alpine:3.20

EXPOSE 8000

WORKDIR /root/

# Copy the Pre-built binary file from the previous stage
COPY --from=builder /app/cmd/main .

# Command to run the executable
ENTRYPOINT ["./main"]