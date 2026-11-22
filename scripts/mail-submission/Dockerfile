FROM scratch
COPY --chmod=0555 mail-submission /usr/local/bin/mail-submission
COPY licenses/ /usr/local/share/licenses/mail-submission/
USER 65532:65532
HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/mail-submission"]
