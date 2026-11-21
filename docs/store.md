# Store: keys and the IAM permissions each one needs

The store is a local directory or an S3 bucket that every machine sharing it reads and writes.
A local store needs no setup beyond the directory.
An S3 store needs IAM permissions on each key prefix recall uses, and those permissions depend on what the machine does: archive, learn, or reduce.

## Every machine writes and reads its own prefixes

| Prefix | Written by | Read by | Holds |
|---|---|---|---|
| `<harness>/<host>/<year>/W<week>.tar.zst` | every archiving host, never overwritten | `learn map --archives`, and the unobserved-host warning | one week of one host's transcripts |
| `analysis/<harness>/…` | every learning host, never overwritten | every learning host | map observations, pull-request evidence, and native memory notes |
| `analysis/hits/<harness>/…` | every learning host, never overwritten | every learning host | rule hits |
| `analysis/reviews/<host>/…` | a host that isn't the reducer, after a forced reduce and apply | the reducer, which deletes each one once it's folded into the ledger | reviews the store ledger doesn't carry yet |
| `learn/reducer` | `recall init` on an unclaimed store, a reduce on an unclaimed store, and `learn reducer --take` | every learning host | the one host that reduces |
| `ledger/<timestamp>Z` | the reducer, never overwritten | every learning host, newest first | versions of the rule ledger |

`<harness>` is `claude` or `kiro`.
Uploads that never overwrite use `If-None-Match: *`; IAM can't require that header, so the policy can't enforce it.

## IAM actions follow from the calls recall makes

- `s3:ListBucket` on the bucket, for every prefix a host lists.
- `s3:PutObject` for every write.
- `s3:GetObject` for every read, and for every existence check: recall skips work it finds already stored with `HeadObject`, which S3 authorizes as `s3:GetObject`.
  Without it, the check fails with 403 rather than 404.
- `s3:DeleteObject` only on `analysis/reviews/*`, and only for the reducer.
  Nothing else in the store is ever deleted.

## Each role adds to the one before it

| Role | Needs |
|---|---|
| Archive | `s3:ListBucket` on `<harness>/*`; `s3:PutObject` on `<harness>/*` |
| Learn | the archive grants, plus `s3:ListBucket` on `analysis/*` and `ledger/*`; `s3:GetObject` and `s3:PutObject` on `analysis/*`; `s3:GetObject` on `ledger/*` and `learn/reducer`; `s3:GetObject` on `<harness>/*` for `learn map --archives` |
| Reduce | the learn grants, plus `s3:PutObject` on `learn/reducer` and `ledger/*`; `s3:DeleteObject` on `analysis/reviews/*` |

A store has one reducer, so only one machine needs the reduce grants.
Giving every machine the same reducer policy is simpler, at the cost of letting any of them claim the reducer or publish a ledger.

A missing grant on the reduce path doesn't stop a run.
The claim and the ledger write each print a warning and the run continues, so a reducer without these grants proposes and applies every week while the store's ledger and claim never change.
After a first reduce, `recall learn reducer` should name the host, and the bucket should hold an object under `ledger/`.

## The printed policies cover setup and archiving only

`recall archive-chats policy setup` prints the one-time policy for creating and configuring the bucket.
`recall archive-chats policy upload` prints the archive role's policy, with no read or delete.
`recall archive-chats check` probes only that archive role: a list and a write under `<harness>/<host>/`.
Neither covers the learn or reduce grants above; add those by hand.

This is a reducer policy for a bucket named `example-bucket` with one harness, `claude`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ListRecallPrefixes",
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::example-bucket",
      "Condition": {"StringLike": {"s3:prefix": ["claude/*", "analysis/*", "ledger/*"]}}
    },
    {
      "Sid": "ReadRecallObjects",
      "Effect": "Allow",
      "Action": "s3:GetObject",
      "Resource": [
        "arn:aws:s3:::example-bucket/claude/*",
        "arn:aws:s3:::example-bucket/analysis/*",
        "arn:aws:s3:::example-bucket/ledger/*",
        "arn:aws:s3:::example-bucket/learn/reducer"
      ]
    },
    {
      "Sid": "WriteRecallObjects",
      "Effect": "Allow",
      "Action": "s3:PutObject",
      "Resource": [
        "arn:aws:s3:::example-bucket/claude/*",
        "arn:aws:s3:::example-bucket/analysis/*"
      ]
    },
    {
      "Sid": "ReducerWrites",
      "Effect": "Allow",
      "Action": "s3:PutObject",
      "Resource": [
        "arn:aws:s3:::example-bucket/ledger/*",
        "arn:aws:s3:::example-bucket/learn/reducer"
      ]
    },
    {
      "Sid": "ReducerPrunesFoldedReviews",
      "Effect": "Allow",
      "Action": "s3:DeleteObject",
      "Resource": "arn:aws:s3:::example-bucket/analysis/reviews/*"
    }
  ]
}
```

For a machine that learns but doesn't reduce, drop the last two statements.
For one that only archives, use `recall archive-chats policy upload`.
