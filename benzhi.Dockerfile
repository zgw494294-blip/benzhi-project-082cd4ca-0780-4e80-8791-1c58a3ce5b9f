FROM --platform=$TARGETPLATFORM golang:1.22
WORKDIR /app
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=$GOPROXY
COPY go.mod go.sum ./
RUN --mount=type=cache,id=benzhi-go-mod,target=/go/pkg/mod     GOTOOLCHAIN=local go mod download
COPY . .
RUN --mount=type=cache,id=benzhi-go-mod,target=/go/pkg/mod     --mount=type=cache,target=/root/.cache/go-build     GOTOOLCHAIN=local go build ./...
CMD ["bash"]
