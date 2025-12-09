之前的版本，buffer过小，对于vps不友好，监控rclone缓存，防止爆缓存


提供我用的rclone参数：

nohup rclone mount sp:DL /root/DL/downloads \
  --config=/root/.config/rclone/rclone.conf \
  --vfs-cache-mode writes \
  --vfs-cache-max-size 20G \
  --vfs-cache-max-age 1h \
  --vfs-write-back 30s \
  --vfs-read-chunk-size 128M \
  --buffer-size 32M \
  --transfers 6 \
  --checkers 6 \
  --tpslimit 8 \
  --tpslimit-burst 8 \
  --user-agent "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36" \
  --no-modtime \
  --umask 000 \
  --allow-other \
  --rc \
  --rc-addr 127.0.0.1:5572 \
  --rc-no-auth \
  --log-file /root/rclone.log \
  --log-level INFO >/dev/null 2>&1 &

  build：
  set GOOS=Linux
  set GOARCH=amd64
  set CGO_ENABLED=0
  go build -o re-asmr-spider-linux -ldflags="-s -w"
