# v1.5 aihub — single static binary, zero external deps (SQLite embedded).
# All state lives under /data (mount ONE volume): aihub.db + git/ + memory/.
FROM debian:bookworm-slim
COPY aihub /aihub
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/aihub", "-data-dir", "/data", "-listen", ":8080"]
