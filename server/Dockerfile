# build stage：有完整 Go toolchain，只負責編譯，不會進最終 image
FROM golang:1.27.1 AS build
WORKDIR /src
# 先只複製 go.mod / go.sum 再下載依賴：這兩個檔案很少變，這一層可以被 cache
COPY go.mod go.sum ./
RUN go mod download
# 原始碼常常變動，所以放在依賴之後
COPY . .
# CGO_ENABLED=0：runtime 用的 distroless/static 沒有 libc，動態連結的執行檔會在啟動時報
# `exec /server: no such file or directory`，所以要編成靜態執行檔
# migrations 已用 go:embed 包進執行檔，runtime 不用再複製 migrations 目錄
RUN CGO_ENABLED=0 go build -trimpath -o /out/server ./cmd/server

# runtime stage：只有執行檔 + CA 憑證、tzdata，沒有 shell，且以 non-root 執行
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
