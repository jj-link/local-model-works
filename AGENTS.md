# Local Model Works agent instructions

## Authenticated browser verification

- Drive the operator console through the user's existing Chrome tab using the OMP Browser Relay extension.
- Reuse the existing `https://jjlink-pc.tail90c6fe.ts.net:9000/` tab; do not create a separate browser profile.
- If the tab is at `/login`, mint a local one-use token with:
  `wsl.exe -d NVIDIA-Workbench -u root -- /home/workbench/Projects/personal/local-model-works/bin/lmw admin browser-login --state /var/lib/local-model-works`
- Within 60 seconds, POST the returned token as JSON to `/api/v1/browser-login` from that same-origin Chrome tab, store the response's `csrf_token` in `sessionStorage` under `lmw.csrf`, then navigate to the required page.
- The token is single-use. Never persist it, place it in a URL, or request/store the operator password.
- Mint a token only when authenticated browser verification is required and the existing session is unavailable.
