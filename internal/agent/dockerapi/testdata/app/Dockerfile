# The integration tests' app image (CI builds it and pushes it to a local
# registry:2 on 127.0.0.1:5000). A static page on 8080, nothing else.
FROM busybox:1
RUN mkdir /www && echo ok > /www/index.html
EXPOSE 8080
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
