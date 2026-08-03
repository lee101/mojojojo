# Mojojojo product introduction

`drip_config.json` defines six opt-in messages over 21 days. Each message has an
HTML fragment and a plain-text equivalent. The application supplies the branded
shell, postal address, and signed unsubscribe URL.

## Safety model

- The signup checkbox is unchecked by default.
- Existing accounts are not enrolled retroactively.
- Every `(campaign, email, user)` send is reserved in PostgreSQL before SMTP;
  successful messages cannot be sent twice.
- Failed attempts wait at least one hour and stop after three attempts.
- Unsubscribing updates the shared preference before any later message is due.
- Links contain campaign/content UTMs but no recipient identifier; there is no
  tracking pixel.
- Delivery is off unless `EMAIL_DRIP_ENABLED=true`.

## Production configuration

The worker refuses to start when enabled unless all of these exist:

```text
AWS_REGION
AWS_SMTP_USERNAME
AWS_SMTP_PASSWORD
SES_FROM_EMAIL
SES_FROM_NAME
SES_REPLY_TO
EMAIL_UNSUBSCRIBE_SECRET
EMAIL_POSTAL_ADDRESS
```

Use a stable random value of at least 32 characters for the unsubscribe secret.
Changing it invalidates links in messages already delivered. `EMAIL_POSTAL_ADDRESS`
must be the real business mailing address; do not use a placeholder.

After changing the environment, restart `mojojojo.service`. The worker checks
for due messages after ten seconds and every fifteen minutes after that. Its
startup log states either `staged, delivery disabled` or the enabled campaign ID.

SES sending identities, DKIM, account-level suppression, and bounce/complaint
notifications are AWS account configuration and are not stored in this repository.
