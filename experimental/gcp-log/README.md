TODO(jayhou): Add more about what the log looks like and how to play with it after deploying.

Note: this page is under construction.

---

# Serverless Log on GCP

This directory contains another example implementation of a log represented by tiles and files, using components from the Trillian repo (e.g. compact ranges) and GCP infrastructure. It demonstrates the usage of [Google Cloud Functions](https://cloud.google.com/functions) to process and integrate new log entries and the usage of [Google Cloud Storage](https://cloud.google.com/storage) as the storage for tiles and files.

## Overview

We deploy two functions to GCF:

1. `integrate`: with the `--initialise` flag, creates a bucket which acts as our log storage layer. Without it, integrates sequenced log entries to the tree by updating the tiles and checkpoint files.
1. `sequence`: assigns leaf index numbers to each new log entry, preparing it for integration to the tree.

Both functions are HTTP-triggered and run when their respective endpoints are requested.

## Deployment

### Pre-reqs

1. Create a GCP project. [Enable](https://cloud.google.com/endpoints/docs/openapi/enable-api) GCF and GCS APIs.

### GCF function deployment

1. Set `PROJECT_NAME` as the name of your GCP project (string, not number).
1. Deploy the Integrate function:

    ```bash
    gcloud functions deploy integrate \
    --entry-point Integrate \
    --runtime go120 \
    --trigger-http \
    --set-env-vars "GCP_PROJECT=${PROJECT_NAME}" \
    --source=./experimental/gcp-log \
    --max-instances 1
    ```

1. Deploy the Sequence function:

    ```bash
    gcloud functions deploy sequence \
    --entry-point Sequence \
    --runtime go120 \
    --trigger-http \
    --set-env-vars "GCP_PROJECT=${PROJECT_NAME}" \
    --source=./experimental/gcp-log \
    --max-instances 1
    ```

1. Grant GCF service account GCS access:

    ```bash
    gcloud projects add-iam-policy-binding serverless-log \
    --member=serviceAccount:serverless-log@appspot.gserviceaccount.com \
    --role=roles/storage.admin \
    --condition=None
    ```

### Write to log

Set up a log and write to the log via GCF invocation.

1. Initialize a log:
   (Add a `"createBucket": true` line to the request data below if you want the function to attempt to create the bucket)

    ```bash
    gcloud functions call integrate \
    --data '{
        "initialise": true,
        "origin": "${ORIGIN}",
        "bucket": "${LOG_NAME}",
        "kmsKeyRing": "${KMS_KEY_RING}",
        "kmsKeyName": "${KMS_KEY_NAME}",
        "kmsKeyVersion": ${KMS_KEY_VERSION}, 
        "kmsKeyLocation": "${KMS_KEY_LOCATION}",
        "noteKeyName": "${NOTE_SIGNING_NAME}"
    }'
    ```

1. Add entries to the bucket:
1. Sequence entries:

    ```bash
    gcloud functions call sequence --data '{
        "entriesDir": "${ENTRIES_BUCKET}",
        "origin": "${ORIGIN}",
        "bucket": "${LOG_NAME}"
    }'
    ```

1. Integrate entries:

    ```bash
    gcloud functions call integrate \
    --data '{
        "origin": "${ORIGIN}",
        "bucket": "${LOG_NAME}",
        "kmsKeyRing": "${KMS_KEY_RING}",
        "kmsKeyName": "${KMS_KEY_NAME}",
        "kmsKeyVersion": ${KMS_KEY_VERSION}, 
        "kmsKeyLocation": "${KMS_KEY_LOCATION}",
        "noteKeyName": "${NOTE_SIGNING_NAME}"
    }'
    ```

### Cache-control

The following two optional parameters can be added to all function calls to customise the
`Cache-Control` headers associated with the log artefacts:

* `checkpointCacheControl`, if supplied, sets the `Cache-Control` header for the `checkpoint` object.
* `otherCacheControl`, if supplied, sets the `Cache-Control` header for all other objects.

The values for these parameters should be a valid [Cache-Control](https://cloud.google.com/storage/docs/metadata#cache-control) metadata string, e.g. `public, max-age=3600`.