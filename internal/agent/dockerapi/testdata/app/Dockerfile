# The integration tests' app image (CI builds it and pushes it to a local
# registry:2 on 127.0.0.1:5000). A static page on 8080, and /cgi-bin/id, which
# answers GET and POST (busybox httpd answers a POST to a static file with 501)
# with "app-<hostname>": the container's own id, so a test can tell which
# container answered. The script reads the request body (Content-Length bytes)
# before it answers: busybox httpd closes the connection as soon as the script
# is done, and a proxy that is still writing the body of a POST to it would see
# the connection fail and abort the response (measured, ~1 in 4,000 requests).
# The container prints one line at start ("sd-testapp ready <hostname>", then
# httpd takes over as PID 1), so a deploy log has a known line of output to keep.
# busybox:1, pinned by digest (the multi-arch index).
FROM busybox:1@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4
RUN mkdir -p /www/cgi-bin && echo ok > /www/index.html \
 && printf '#!/bin/sh\nhead -c "${CONTENT_LENGTH:-0}" >/dev/null\necho "Content-type: text/plain"\necho\necho "app-$(hostname)"\n' > /www/cgi-bin/id \
 && chmod +x /www/cgi-bin/id
EXPOSE 8080
CMD ["sh", "-c", "echo \"sd-testapp ready $(hostname)\"; exec httpd -f -p 8080 -h /www"]
