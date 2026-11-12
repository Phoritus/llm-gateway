# 16 Check real mail delivery before a production deploy

Status: ready-for-human

The SMTP path has no end-to-end test; `auth-fix-list.md` leaves it as a
manual check. Before deploying, set `SMTP_URL`, `MAIL_FROM` and an https
`PUBLIC_BASE_URL` in the deployment's settings, sign up, and confirm that the
verification and reset links arrive and work.

## Comments
