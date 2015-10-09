# What is it?

BNP Paribas does not let you export all operations from a given account, only
for the last three months. But they provide monthly PDF reports for the whole
account history. `bnp` let you extract operations from such reports and offers
a simple way to chart the results.

Use:
```
bnp parse --json account.json *.pdf
```
to extract operations from input PDF reports, print them on stdout and serializer them as JSON in account.json.

Then:
```
bnp web account.json
```
starts a web server on localhost:8081 (see `--http`) and charts the result.

I wish they offered this service themselves.
