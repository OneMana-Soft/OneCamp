# Publishing this edition

This branch is the AI-free edition. It is the one intended to be published
under Apache-2.0, which makes OneCamp open core: an open server anyone can read
and run, and a commercial edition for the AI teammates.

It is not published yet, and the reason is in this file rather than in
somebody's head.

## Why this cannot be a mirror of the private repository

`git push` carries reachable history, and this repository's history carries
secrets. Forty-five distinct secret-named variables appear in committed files
across the log, including `AI_CONFIG_KEK`, which encrypts every stored
credential in a running workspace, along with provider keys, OAuth client
secrets and database passwords. `shared.env` alone appears in thirteen commits.

Publishing the history would put all of it in public permanently, and rewriting
history in place would leave every existing clone and tag inconsistent.

So publication is a **fresh repository from a squashed tree**: one commit, no
history, nothing to excavate.

## Before publishing

1. **Rotate every secret that has ever been committed.** They are compromised
   whether or not this is published, because they sit in a repository and on a
   server. Publication only changes who can read them.
2. **Check the tree, not just the history.** `vars/.env.prod` is shipped to
   customers as `.sample.env`, so whatever it holds is already in every
   customer's hands. `make secrets` regenerates the generated ones on install;
   confirm nothing in it is a real credential before it becomes public.
3. **Decide the repository name.** The frontend is `OneMana-Soft/OneCamp-fe`,
   so `OneMana-Soft/OneCamp` is the obvious home.

## Publishing

```sh
# From a clean checkout of without-ai, with secrets rotated:
git checkout without-ai
git rm -r --cached vars                      # never ship real env files
git checkout --orphan publish                # no parent, therefore no history
git add -A
git commit -m "OneCamp, AI-free edition, under Apache-2.0"

gh repo create OneMana-Soft/OneCamp --public \
  --description "OneCamp: self-hosted workspace (chat, tasks, docs, calls, calendar). Go backend, AI-free edition." \
  --homepage https://onemana.dev
git remote add public git@github.com:OneMana-Soft/OneCamp.git
git push public publish:main
```

Then set the topics, the same way the frontend repository has them, so the
result is discoverable rather than merely present.

## After publishing

`awesome-selfhosted` requires source, a licence and working installation
instructions, all three of which this then has. That listing is the single
highest-traffic free channel for a product of this kind and it is closed to a
project without a published server.
