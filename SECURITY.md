# Security

## Reporting a vulnerability

Report privately through GitHub's
[security advisories](https://github.com/RedaEkengren/FreeSMS/security/advisories/new).
Do not open a public issue for anything that could be exploited against a
running installation.

Please include what you did, what happened, and what you expected. A proof of
concept helps; access to somebody else's data does not — describe the path
rather than collecting what it reaches.

You will get an acknowledgement within seven days. This is a small project run
by one person, so a fix may take longer than that; you will be told where it
stands rather than left waiting.

## What this project holds

A workshop installation contains personal data: customer names, addresses and
contact details, vehicle registrations, and photographs taken inside a
workshop. Anything that lets one shop see another's data, or one role see data
outside its permissions, is treated as a serious vulnerability regardless of
how it is reached.

## Scope

In scope: this repository's code, its container images, and its default
configuration.

Out of scope: vulnerabilities in a self-hoster's own deployment, network or
operating system; findings that require access the attacker was already given;
and reports produced by a scanner without a demonstrated path to impact.
