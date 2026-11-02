#!/bin/sh
# A greeting, EHLO and NOOP only: no credentials, sender, recipient or body.
set +x
set -eu
status=0
reply=$(curl --disable --proto '=smtp' --noproxy '*' --max-redirs 0 \
  --connect-timeout 5 --max-time 10 --silent --request NOOP \
  --output /dev/null --write-out '%{response_code}' \
  'smtp://mx01.mail.icloud.com:25/email-transport-probe.invalid' 2>/dev/null) || status=$?
if [ "$status" = 0 ] && [ "$reply" = 250 ]; then
  printf '%s\n' 'SMTP_PROVIDER_SESSION=VERIFIED'
else
  # Curl's fixed numeric result is useful without exposing the SMTP banner.
  printf 'SMTP_PROVIDER_SESSION=UNVERIFIED curl_exit=%s response=%s\n' "$status" "$reply"
  exit 3
fi
