FROM golang:1.27.1
WORKDIR /app
# download depenedency first
# 因為變動比較少，所以提早拉出來做cache
COPY go.mod  go.sum ./
RUN go mod download
# 此時還是會COPY go.mod跟go.sum，
# 但因為這包同時包含code changes，所以很難cache
COPY . . 
# 編譯產生執行檔
RUN go build -o main .
# 執行server application
CMD ["./main"]