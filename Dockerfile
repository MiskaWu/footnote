# footnote 的容器映像。設定檔不進映像，掛到 FOOTNOTE_CONFIG 指的路徑；
# 密鑰只走環境變數（DISCORD_BOT_TOKEN、ANTHROPIC_API_KEY、FOOTNOTE_DATABASE_URL、FOOTNOTE_GIT_*）。
#
# 本機試跑：podman build -t footnote:dev . 然後見 README「容器」一節。

FROM docker.io/library/golang:1.25.11-alpine3.22 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/footnote ./cmd/footnote

FROM docker.io/library/alpine:3.22.7
# git ≥ 2.46：讀檔時用 attr.tree 從版本裡取 .gitattributes（internal/source）；git-lfs：文件的 xlsx 可能放在 LFS。
RUN apk add --no-cache ca-certificates git git-lfs tzdata \
 && git lfs install --system --skip-repo \
 # https 的 clone／fetch／LFS 下載都向這個 helper 要帳密，值來自環境變數（GitLab deploy token），
 # 不寫進 remote URL，也就不會落在 PVC 上的 .git/config。
 && git config --system credential.helper \
      '!f() { test "$1" = get || exit 0; echo "username=${FOOTNOTE_GIT_USERNAME}"; echo "password=${FOOTNOTE_GIT_TOKEN}"; }; f' \
 # 資料夾在 PVC 上，擁有者可能因 fsGroup 與執行身分不同；容器裡只有這一個程式，不需要這道多使用者防護。
 && git config --system safe.directory '*' \
 && adduser -D -u 10001 -h /home/footnote footnote \
 && mkdir -p /data && chown footnote:footnote /data
COPY --from=build /out/footnote /usr/local/bin/footnote
USER footnote
ENV FOOTNOTE_CONFIG=/etc/footnote/footnote.yaml \
    TZ=Asia/Taipei
EXPOSE 28090
ENTRYPOINT ["footnote"]
CMD ["serve"]
