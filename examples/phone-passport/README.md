# Phone Passport

Paste a messy list of international phone numbers and normalize them to E.164,
international, and national formats with region and validity information. The
backend prefers the adjacent
[`mojo-phonenumbers`](https://github.com/lee101/mojo-phonenumbers) port and
falls back to the upstream Python package while it warms.

[![Deploy to mojojojo](https://mojojojo.cc/static/deploy-button.svg)](https://mojojojo.cc/studio?example=phone-passport&kind=tool)

The function accepts at most 50 lines and does not persist input. It is a handy
starting point for lead cleanup, contact import previews, and signup-form
validation. License: MIT.
