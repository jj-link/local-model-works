# Local Model Works agent instructions

## Authenticated browser verification

- Drive the operator console through the user's existing Chrome tab using the OMP Browser Relay extension.
- If the tab is at `/login`, mint a local one-use token with:
  `wsl.exe -d NVIDIA-Workbench -u root -- /home/workbench/Projects/personal/local-model-works/bin/lmw admin browser-login --state /var/lib/local-model-works`
- Within 60 seconds, POST the returned token as JSON to `/api/v1/browser-login` from that same-origin Chrome tab, store the response's `csrf_token` in `sessionStorage` under `lmw.csrf`, then navigate to the required page.
- The token is single-use. Never persist it, place it in a URL, or request/store the operator password.
- Mint a token only when authenticated browser verification is required and the existing session is unavailable.

## Model storage on the Spark fleet

- Store every model file once, on one Spark. The other Sparks read it from that Spark over the 200G switch (a shared folder on `10.10.0.x`); never copy model weights to each Spark's SSD.
- Download from the internet once, on one Spark. Never download the same files on several Sparks.
- This applies to every recipe, even when the recipe's own docs say to put a copy on each machine.
