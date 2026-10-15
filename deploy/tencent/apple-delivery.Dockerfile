FROM scratch
LABEL cn.tellyouwhat.operations-automation="1"
LABEL cn.tellyouwhat.lifecycle="1"
COPY service /service
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/service"]
