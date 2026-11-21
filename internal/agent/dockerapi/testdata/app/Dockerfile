# The integration tests' app image (CI builds it and pushes it to a local
# registry:2 on 127.0.0.1:5000). A static page on 8080, nothing else.
# busybox:1, pinned by digest (the multi-arch index).
FROM busybox:1@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4
RUN mkdir /www && echo ok > /www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
