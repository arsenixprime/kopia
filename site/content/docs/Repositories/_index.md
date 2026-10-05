---
title: "Repositories"
linkTitle: "Supported Storage Locations"
weight: 25
---

Kopia allows you to save your [encrypted](../features/#user-controlled-end-to-end-encryption) backups (which are called [`snapshots`](../faqs/#what-is-a-snapshot) in Kopia) to a variety of storage locations, and in Kopia a storage location is called a `repository`. Kopia supports all of the following storage locations:

> PRO TIP: You pick the storage locations you want to use. Kopia plays no role in selecting your storage locations. This means you must provision, setup, and pay (the storage provider) for whatever storage locations you want to use **before** you create a `repository` for that storage location in Kopia.

* [Amazon S3 and S3-compatible Cloud Storage](#amazon-s3-and-s3-compatible-cloud-storage)
  * Kopia supports all cloud storage platforms that support the S3 API
  * Kopia supports object locking and [hot, cold, and archive storage classes](../advanced/amazon-s3/) for any cloud storage that supports the features using the S3 API
* [Azure Blob Storage](#azure-blob-storage)
* [Backblaze B2](#backblaze-b2)
* [Google Cloud Storage](#google-cloud-storage)
* [Google Drive](#google-drive)
  * Kopia supports Google Drive natively and through Kopia's Rclone option (see below)
  * Native Google Drive support is currently only available through Kopia CLI
  * Native Google Drive support operates differently than Kopia's support for Google Drive through Rclone; you will not be able to use the two interchangeably, so pick one
* All remote servers or cloud storage that support [WebDAV](#webdav) 
* All remote servers or cloud storage that support [SFTP](#sftp)
* Some of the cloud storages supported by [Rclone](#rclone) 
  * Rclone is a (free and open-source) third-party program that you must download and setup separately before you can use it with Kopia
  * Once you setup Rclone, Kopia automatically manages and runs Rclone for you, so you do not need to do much beyond the initial setup, aside from enabling Rclone's self-update feature so that it stays up-to-date
  * Kopia's Rclone support is experimental: not all the cloud storages supported by Rclone have been tested to work with Kopia, and some may not work with Kopia; Kopia has been tested to work with [Dropbox](#rclone), [OneDrive](#rclone), and [Google Drive](#rclone) through Rclone
* Your local machine and any network-attached storage or server 
* Your own remote server by setting up a [Kopia Repository Server](../repository-server/)

> PRO TIP: Many cloud storage providers offer a variety of [storage tiers](../advanced/storage-tiers/) that may (or may not) help decrease your cost of cloud storage, depending on your use case. See the [storage tiers documentation](../advanced/storage-tiers/) to learn the different types of files Kopia stores in repositories and which one of these file types you can possibly move to archive tiers, such as Amazon Deep Glacier.

## Amazon S3 and S3-compatible Cloud Storage

Creating an Amazon S3 or S3-compatible storage `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

### Kopia GUI

Select the `Amazon S3 and Compatible Storage` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `Bucket` name, `Server Endpoint`, `Access Key ID`, and `Secret Access Key`. You can optionally enter an `Override Region` and `Session Token`.

> NOTE: Some S3-compatible cloud storage may have slightly different names for bucket, endpoint, access key, secret key, region, and session token. This will vary between cloud storages. Read the help documentation for the cloud storage you are using to find the appropriate values. You can typically find this information by searching for the S3 API settings for your cloud storage.

You will next need to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as object locking and [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create s3` command](../reference/command-line/common/repository-create-s3/) to create a `repository`:

```shell
$ kopia repository create s3 \
        --bucket=... \
        --access-key=... \
        --secret-access-key=...
```

At a minimum, you will need to enter the bucket name, access key, and secret access key. If you are not using Amazon S3 and are using an S3-compatible storage, you will also need to enter the endpoint and may need to enter the `region`. There are also various other options (such as object locking and [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-s3/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect s3` command](../reference/command-line/common/repository-connect-s3/). Read the [help docs](../reference/command-line/common/repository-connect-s3/) for more information on the options available for this command.

## Azure Blob Storage

Creating an Azure Blob Storage `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

### Kopia GUI

Select the `Azure Blob Storage` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `Container` name, `Storage Account` name and either `Access Key` or `SAS Token`. You can optionally enter an `Azure Storage Domain`.

You will next need to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create azure` command](../reference/command-line/common/repository-create-azure/) to create a `repository`:

```shell
$ kopia repository create azure \
        --container=... \
        --storage-account=... \
        --storage-key=...
```

OR

```shell
$ kopia repository create azure \
        --container=... \
        --storage-account=... \
        --sas-token=...
```

At a minimum, you will need to enter the container name, storage account name, and either your Azure account access key/storage key or a SAS token. There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-azure/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect azure` command](../reference/command-line/common/repository-connect-azure/). Read the [help docs](../reference/command-line/common/repository-connect-azure/) for more information on the options available for this command.

## Backblaze B2

Creating a Backblaze B2 `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

> NOTE: Currently, object locking is supported for B2 but only through Kopia's [S3-compatible storage `repository`](#amazon-s3-and-s3-compatible-cloud-storage) and not through the B2 `repository` option. However, B2 is fully S3 compatible, so you can setup your B2 account via Kopia's [S3 `repository` option](#amazon-s3-and-s3-compatible-cloud-storage). To use B2 storage with the S3 `repository` option the `--endpoint` argument must be specified with the appropriate B2 endpoint. This endpoint can be found on the buckets page of the B2 web interface and follows the pattern `s3.<region>.backblazeb2.com`.

### Kopia GUI

Select the `Backblaze B2` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `B2 Bucket` name, `Key ID`, and application `Key`.

You will next need to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create b2` command](../reference/command-line/common/repository-create-b2/) to create a `repository`:

```shell
$ kopia repository create b2 \
        --bucket=... \
        --key-id=... \
        --key=...
```

There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-b2/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect b2` command](../reference/command-line/common/repository-connect-b2/). Read the [help docs](../reference/command-line/common/repository-connect-b2/) for more information on the options available for this command.

## Google Cloud Storage

Creating a Google Cloud Storage `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

### Kopia GUI

Select the `Google Cloud Storage` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter the `GCS Bucket` name and enter the path to where on your machine you have saved the Google Cloud Storage `Credentials File`. The credentials file can be obtained by [creating a Google Cloud Service Account](https://cloud.google.com/docs/authentication/getting-started#create-service-account-console) that allows you to access your storage bucket and then downloading the JSON key file for that service account. You enter the path to this JSON key file in the `Credentials File` textbox in `KopiaUI`.

You will next need to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

There are three methods to create a `repository` for Google Cloud Storage: one that requires you to install Google Cloud SDK; the other method allows you to generate credentials without Google Cloud SDK; and the third method allows you to use Google Cloud Storage through Kopia's [S3 `repository` option](#amazon-s3-and-s3-compatible-cloud-storage):
##### Method #1: Installing Google Cloud SDK

1. Create a storage bucket in [Google Cloud Console](https://console.cloud.google.com/storage/)
2. Install [Google Cloud SDK](https://cloud.google.com/sdk/)
3. Log in with credentials that have permissions to the bucket

```shell
$ gcloud auth application-default login
```

After these preparations, we can create a Kopia `repository` (assuming bucket named `kopia-test-123`) using the [`kopia repository create gcs` command](../reference/command-line/common/repository-connect-gcs/):

```shell
$ kopia repository create gcs --bucket kopia-test-123
```

There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-gcs/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

##### Method #2: Creating a Service Account and Using the JSON Key File

1. Create a storage bucket in [Google Cloud Console](https://console.cloud.google.com/storage/)
2. Create a Google Cloud Service Account that allows you to access your storage bucket. Directions are available on [Google Cloud's website](https://cloud.google.com/authentication/getting-started#create-service-account-console). Make sure to download the JSON key file for your service account and keep it safe.

After these preparations, we can create a Kopia `repository` (assuming bucket named `kopia-test-123`) using the [`kopia repository create gcs` command](../reference/command-line/common/repository-connect-gcs/):

```shell
$ kopia repository create gcs --credentials-file="/path/to/your/credentials/file.json" --bucket kopia-test-123
```

There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-gcs/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

##### Method #3: Enabling Amazon S3 Interoperability in Google Cloud Storage

1. Create a storage bucket in [Google Cloud Console](https://console.cloud.google.com/storage/)
2. Go to [Settings and then Interoperability](https://console.cloud.google.com/storage/settings;tab=interoperability) in your Google Cloud Storage account
3. Enable your project under `Default project for interoperable access` and generate access keys for this project -- you will generate both access key and secret key, just like if you were using Amazon S3

After these preparations, we can create a Kopia `repository` (assuming bucket named `kopia-test-123`) using the [`kopia repository create s3` command](../reference/command-line/common/repository-connect-s3/):

```shell
$ kopia repository create s3 --endpoint="storage.googleapis.com" --bucket="kopia-test-123" --access-key="access/key/here" --secret-access-key="secret/key/here"
```

There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-s3/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect gcs` command](../reference/command-line/common/repository-connect-gcs/) or the [`kopia repository connect s3` command](../reference/command-line/common/repository-connect-s3/), depending on whichever way you setup the Google Cloud Storage `repository`. Read the [help docs for `repository connect gcs`](../reference/command-line/common/repository-connect-gcs/) or the [help docs for `repository connect s3`](../reference/command-line/common/repository-connect-s3/) for more information on the options available for these commands.

### Credential permissions

The following permissions are required when in readonly mode:
```
storage.buckets.get
storage.objects.get
storage.objects.list
```

When in normal read-write mode the following additional permissions are required:
```
storage.objects.update
storage.objects.create
storage.objects.delete
```

If using [ransomware protection](../advanced/ransomware#Google-protection) then the following additional permission is required:
```
storage.objects.setRetention
```

## Google Drive

Kopia supports Google Drive in two ways: natively and through Kopia's [Rclone `repository` option](#rclone). Native Google Drive support is currently only available through Kopia CLI; Kopia GUI users need to use Kopia's [Rclone `repository` option](#rclone).

Kopia uses a Google Drive folder to store all the files in the `repository`. Kopia will only access files in this folder, and using Kopia does not impact your other Google Drive files. It is recommended that you let Kopia manage this folder and do not upload any other content to this folder. Kopia can create the folder for you (`--create-folder-name`), which is the recommended way to set up the first machine; you can also point it at a folder that already exists (`--folder-id`).

Before you create a Google Drive `repository`, you have to decide how Kopia signs in to Google. There are three ways, and the choice matters a lot -- it decides who owns the backup files, whose storage quota they consume, and how much of your Drive Kopia is able to see:

| Sign-in method | Who owns the files | Use it when |
| --- | --- | --- |
| [Your own Google account](#option-1-sign-in-as-yourself-recommended) (recommended) | you | almost always, including personal Gmail accounts and Google Workspace accounts |
| [A service account impersonating a user](#option-2-service-account-with-domain-wide-delegation) | the impersonated user | you administer a Google Workspace domain and want unattended backups with no interactive sign-in |
| [A bare service account](#option-3-a-bare-service-account) | the service account | you are writing into a Shared Drive; see the storage warning below |

All three need a Google Cloud project of your own with the Google Drive API enabled. This is unavoidable and it is also a good thing: Google charges API quota to the Cloud project that owns the credentials, so your own project gets its own quota that nobody else can exhaust. One project serves your whole household or organization -- you create it once and every machine reuses the same credentials.

### Kopia CLI

#### Creating a Google Cloud project

Every sign-in method starts here, and you only ever do this once:

1. [Create a Google Cloud project](https://console.cloud.google.com/projectcreate), or use an existing one.

2. [Enable the Google Drive API](https://console.cloud.google.com/apis/library/drive.googleapis.com) for your project. If you skip this step, Kopia fails at connect time with an `accessNotConfigured` error that names the project and links to this page.

#### Option 1: Sign in as yourself (recommended)

With this method Kopia acts as you: the repository lives in your own Drive, the files are owned by you, and they count against the Drive storage you already pay for. You sign in once in a browser and Kopia caches the resulting refresh token, so every later run is silent.

Continuing from the project you created above:

3. Configure the OAuth consent screen (`APIs & Services` -> `OAuth consent screen`). If your account is part of a Google Workspace organization, choose the `Internal` user type: an internal app is usable only by accounts in your own organization and does **not** need to go through Google's app verification. If you have a personal Gmail account, `Internal` is not offered; choose `External` and add your own Google account as a test user, which is likewise enough for personal use and does not need verification.

4. Create the OAuth client (`APIs & Services` -> `Credentials` -> `Create credentials` -> `OAuth client ID`). Choose the `Desktop app` application type. Download the resulting JSON file -- it is the document with an `installed` (or `web`) section in it -- and save it on the machine that will run Kopia.

5. Create the `repository`, pointing Kopia at that file and telling it what to call the folder it should make for you:

```shell
$ kopia repository create gdrive \
        --create-folder-name=my-kopia-backups \
        --client-credentials-file="<where-you-have-stored-the-oauth-client-json>"
```

Kopia prints a Google sign-in URL and waits. Open the URL in a browser (on any machine -- see [headless machines](#headless-machines) below), approve the request, and the browser is redirected back to a short-lived local server Kopia runs on `127.0.0.1`, which captures the authorization code and shuts down. Kopia then stores the refresh token in its [token cache](#where-the-sign-in-token-is-stored) and continues creating the repository.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

`--create-folder-name` makes a folder of that name in the root of your Drive (`My Drive`) and puts the repository in it. Kopia prints the ID of the folder it made and saves it in the repository configuration:

```
Created the Google Drive folder "my-kopia-backups", ID 1BTGFd7qG9CWiaJdN6WbUCdTYzdQXEbhr.
Google Drive folder "my-kopia-backups" has ID 1BTGFd7qG9CWiaJdN6WbUCdTYzdQXEbhr - connect other machines with: kopia repository connect gdrive --folder-id 1BTGFd7qG9CWiaJdN6WbUCdTYzdQXEbhr
```

**Write that folder ID down**: it is what every other machine uses to reach the same repository, with `kopia repository connect gdrive --folder-id=...`. You can also read it back at any time with `kopia repository status`.

Running the same `create --create-folder-name` again is safe: if Kopia can already see exactly one folder of that name it reuses it rather than making a second one, and then refuses to create a repository over the data that is already there.

> NOTE: By default Kopia asks Google only for the `drive.file` scope, which lets it see and modify **only the files and folders it created itself** -- it is not able to read the rest of your Drive even if it wanted to. That is exactly why `--create-folder-name` is the recommended way to start: a folder Kopia makes for itself is one it can see. A folder you created by hand in the Drive web UI is *not*, and a `--folder-id` naming such a folder fails at connect time with "unable to open the Google Drive folder", even though you can see the folder perfectly well in your browser. See [choosing a scope](#choosing-a-scope).

> PRO TIP: If you would rather put the repository in a folder you made yourself in the Drive web UI, name it with `--folder-id` instead of `--create-folder-name`, and pass `--scope=drive` on every `create` and `connect` for that repository -- otherwise Kopia cannot see it. `--folder-id` and `--create-folder-name` are mutually exclusive, and `--create-folder-name` is only accepted by `repository create`; there is nothing to create when you are connecting to a repository that already exists.

The same OAuth client JSON works on every machine you back up. Copy it around freely; it is a client identifier, not a password, and each machine performs its own sign-in against it.

> NOTE: The `drive.file` grant is recorded per user, per application, per file. A second person signing in with the same OAuth client but their own Google account does **not** inherit access to the files the first person's Kopia created, even if the folder is shared with them. For a repository shared between several people, use `--scope=drive` and share the folder normally, or give every machine the same credentials.

#### Option 2: Service account with domain-wide delegation

> WARNING: This method is EXPERIMENTAL. The code path is complete and unit-tested, but it has not been validated against a real Google Workspace domain, because doing so requires a Workspace super-admin to authorize the client ID. Please report your experience.

This is the answer for a Google Workspace administrator who wants unattended backups without any interactive sign-in, while still having the files owned by a real user with real storage quota. The service account does not own anything; it acts *on behalf of* a user in your domain.

Continuing from the project you created above:

3. [Create a service account](https://console.cloud.google.com/iam-admin/serviceaccounts) and give it a name. Note down the service account email and its numeric client ID.

4. Create a key for the service account: [view the service account](https://console.cloud.google.com/iam-admin/serviceaccounts), go to the `Keys` tab, click `Add Key` -> `Create new key`, and choose `JSON`. Save the file on the machine that will run Kopia.

5. In the [Google Workspace admin console](https://admin.google.com/), go to `Security` -> `Access and data control` -> `API controls` -> `Domain-wide delegation` and add a new API client. Enter the service account's numeric client ID, and enter `https://www.googleapis.com/auth/drive.file` as the OAuth scope (or `https://www.googleapis.com/auth/drive` if you intend to use `--scope=drive`). This step requires super-admin rights, and it is what makes impersonation legal; without it Google rejects the token request with `unauthorized_client`.

6. Create the `repository`, naming the user to act as:

```shell
$ kopia repository create gdrive \
        --create-folder-name=my-kopia-backups \
        --credentials-file="<where-you-have-stored-the-json-key-file>" \
        --impersonate-user=backups@example.com
```

Everything Kopia does here is done as `backups@example.com`, exactly as if that user had run Kopia: the folder is created in that user's own `My Drive`, everything Kopia writes is owned by them, and it counts against their storage. No browser is involved at any point. Use `--folder-id` instead of `--create-folder-name` if the repository must live in a folder that already exists.

#### Option 3: A bare service account

> WARNING: A service account is its own Drive user with roughly 15 GB of storage that **cannot be increased or purchased**. If you point Kopia at a folder in the service account's own Drive, your backups will stop working when they hit that ceiling. Kopia prints a warning at connect time when it detects this configuration.

This method is only appropriate when the repository lives on a **Shared Drive**, whose storage belongs to the organization rather than to the service account. `--create-folder-name` is not the right tool here -- it creates in the root of `My Drive`, which for a bare service account is the 15 GB drive you are trying to avoid. Follow steps 3 and 4 of [option 2](#option-2-service-account-with-domain-wide-delegation) to create the service account and its JSON key, create a folder on a Shared Drive, add the service account email as a member of that Shared Drive with `Content manager` (or higher) access, and then:

```shell
$ kopia repository create gdrive \
        --folder-id=z63ZZ1Npv3OFvDPwU3dX0w \
        --credentials-file="<where-you-have-stored-the-json-key-file>"
```

If you want the credentials stored inside the Kopia configuration file instead of being read from disk on every run, add `--embed-credentials`.

If this fails with "unable to open the Google Drive folder", the folder is not visible to Kopia under the default scope; add `--scope=drive` to both `create` and `connect`. See [choosing a scope](#choosing-a-scope).

#### Choosing a scope

The `--scope` flag selects which Google Drive OAuth scope Kopia requests:

* `--scope=drive.file` (the default) grants Kopia access to **only the files and folders it created itself**. This is a non-sensitive scope, so an app using it never needs to pass Google's app-verification or third-party security-assessment process. A folder created outside Kopia -- in the Drive web UI, or by another Kopia installation signed in as a different user -- is invisible to Kopia under this scope, and a `files.get` on it returns "not found". `--create-folder-name` avoids the problem entirely: a folder Kopia creates is a folder Kopia can see, so the default scope is enough for the whole life of the repository.
* `--scope=drive` grants full read/write access to **every file in the Drive of the account that signs in**. Use it only when the repository must live in a pre-existing, externally created folder. Note that `drive` is one of Google's restricted scopes, so an app distributed under it would need Google's security assessment; that does not apply to an `Internal` Workspace app or to a personal-use `External` app with test users, but it is the reason `drive.file` is the default.

`--read-only` is orthogonal and takes precedence over both: it requests `drive.readonly` so that the connection cannot mutate anything.

#### Headless machines

The default sign-in requires a browser only to *open a URL*; Kopia never launches one for you. On a machine with no browser, forward the loopback port over SSH (`ssh -L`) and open the printed URL on your laptop -- the redirect will reach the Kopia instance on the remote machine.

If you cannot forward a port either, use `--device-flow`, which prints a short code to type into <https://google.com/device> on any other device. There is one hard limitation, imposed by Google and not by Kopia: **the device flow only permits the `drive.file` scope**. If you need `--scope=drive` on a headless machine, you must complete the sign-in on a machine that can accept a loopback connection and then copy the resulting token cache file (see below) to the headless machine.

#### Where the sign-in token is stored

The refresh token obtained by the interactive sign-in is written to Kopia's configuration directory, in a file named `gdrive-token-<hash>.json` where the hash is derived from the OAuth client ID and the folder ID. That is `~/.config/kopia` on Linux, `~/Library/Application Support/kopia` on macOS, and `%APPDATA%\kopia` on Windows. The file is created owner-readable only (`0600`). Use `--token-cache-file` to put it somewhere else -- for example on removable media, or in a location shared between several repositories.

Deleting the token cache file simply causes the next connection to prompt for sign-in again. Revoking Kopia's access from your [Google account permissions page](https://myaccount.google.com/permissions) invalidates it server-side.

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect gdrive` command](../reference/command-line/common/repository-connect-gdrive/), which takes the same authentication flags as `create`. Connecting always names the folder by ID -- there is no `--create-folder-name` on `connect`, because the folder already exists:

```shell
$ kopia repository connect gdrive \
        --folder-id=1BTGFd7qG9CWiaJdN6WbUCdTYzdQXEbhr \
        --client-credentials-file="<where-you-have-stored-the-oauth-client-json>"
```

That is the ID printed when the repository was created; `kopia repository status` shows it on a machine that is already connected. Read the [help docs](../reference/command-line/common/repository-connect-gdrive/) for more information on the options available for this command.

If you view your folder on Google Drive, you should see that Kopia has created the skeleton of the repository with a `kopia.repository` file and a couple of others. Kopia will store all the files for your snapshots in this folder.

There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-gdrive/) for more information.

#### Practical limits of Google Drive

Google Drive is a consumer file-sync product before it is an object store, and it enforces a handful of limits that a backup tool can actually reach. None of them are Kopia limits and Kopia cannot raise any of them, so plan around them:

* **750 GB uploaded per day, per user.** This is a combined budget across My Drive and every Shared Drive the user writes to; writing into a Shared Drive does not buy a fresh allowance. Once it is reached the user cannot upload anything until the limit refreshes, within 24 hours. This matters mostly when seeding a large repository for the first time: an initial snapshot bigger than 750 GB has to be spread over several days, and Kopia will simply be unable to upload in the meantime. Incremental snapshots are rarely anywhere near this.
* **500,000 items per folder, and 500,000 items per Shared Drive.** Kopia stores the repository as a flat folder of blobs, so both limits are real ceilings on the number of blobs a repository can hold. The per-Shared-Drive cap is the harsher of the two, because sharding into subfolders does not help against it -- a repository that needs more than 500,000 objects cannot live on a single Shared Drive. My Drive has no equivalent whole-account cap below 500 million items. **Items in the trash count against these limits**; Kopia therefore deletes blobs permanently rather than trashing them, so that deleted blobs stop counting immediately. The corollary is that a blob Kopia deletes is not recoverable from your Drive trash -- rely on Kopia's own [maintenance safety margins](../advanced/maintenance/), not on Drive.
* **API quota is measured in quota units, not requests.** Google's current model gives a Cloud project 1,000,000 quota units per minute, and any one user 325,000 quota units per minute within that project; the cost of a call depends on what it does (reading an item costs 5 units, listing costs 100, downloading costs 200, editing costs 50). See Google's [usage limits documentation](https://developers.google.com/workspace/drive/api/guides/limits) for the current numbers. In practice a Kopia backup does not come close: a two-and-three-quarter-hour benchmark run that moved 2.71 GB averaged about 1,393 quota units per minute, or 0.43% of the per-user ceiling. Latency, not quota, is what bounds a Drive-backed repository.
* **Use your own OAuth client.** Both quota buckets above are charged to the Google Cloud project that owns the credentials Kopia signs in with. A client of your own means a quota bucket of your own, which nobody else's traffic can exhaust. This is the single strongest reason to follow the setup above rather than looking for a way to skip it.

#### Performance and tuning

The Google Drive backend has a set of performance knobs -- pacer timing, upload chunk size, list page size, delete parallelism -- exposed both as a `tuning` object in the repository connection configuration and as flags on `kopia repository create gdrive` and `kopia repository connect gdrive`: `--pacer-min-sleep-ms`, `--pacer-burst`, `--pacer-max-sleep-ms`, `--max-tries`, `--upload-chunk-size-mb`, `--simple-upload-cutoff-mb`, `--list-page-size`, `--delete-parallelism`, `--use-batch-delete` and `--tuning-cache-dir`. A knob you do not set on the command line is not written to the configuration at all, so it keeps following the built-in default. **The shipped defaults are the recommendation**: they are the values that measured best (or that no measurement has yet beaten) in interleaved benchmark runs against real Google Drive, and every one of them carries a written provenance note in `tools/gdrive-bench/gdrive-tuned.json` in the Kopia source tree, including which values were tested and rejected. Do not change them speculatively.

Connections that stall -- typically after a laptop resumes from suspend on a different network -- are detected and retried rather than left hanging: HTTP/2 connections are health-checked with a ping after 31 seconds of silence and abandoned if it goes unanswered for 15 seconds, a response that has not started 2 minutes after its request was fully sent is abandoned, and any connection that reads and writes nothing for 5 minutes is closed. The bounds are adjustable with `--http2-read-idle-timeout-sec`, `--http2-ping-timeout-sec`, `--response-header-timeout-sec` and `--io-idle-timeout-sec`; none of them limits how long a transfer that is making progress may take.

The one setting that is worth thinking about is Kopia's [pack size](../advanced/architecture/), because it is a Kopia setting rather than a Drive one. Measurements show that this backend is bound by the number of Drive API round trips rather than by the number of bytes moved: writing a 2-8 KB blob takes about the same wall-clock time as a much larger one, so writing many small objects is disproportionately expensive. Keep the default pack size or raise it; lowering it to make individual uploads smaller will make backups slower, not faster.

> NOTE: Native Google Drive support is exercised by Kopia's standard storage-provider test suite, including its concurrency and provider-validation checks. The domain-wide delegation path described in [option 2](#option-2-service-account-with-domain-wide-delegation) is the exception and is explicitly marked experimental above.

## WebDAV

Creating a WebDAV `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

### Kopia GUI

Select the `WebDAV Server` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `WebDAV Server URL`, `Username`, and `Password.

You will next need to enter the repository password that you want. This password can be whatever you want, it does not need to be the same as your WebDAV password. In fact, it should not be the same! Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create webdav` command](../reference/command-line/common/repository-create-webdav/) to create a `repository`:

```shell
$ kopia repository create webdav \
        --url=... \
        --webdav-password=... \
        --webdav-username=...
```


At a minimum, you will need to enter the WebDAV server URL, username, and password. There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-webdav/) for more information.

You will be asked to enter the repository password that you want. This password can be whatever you want, it does not need to be the same as your WebDAV password. In fact, it should not be the same! Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect webdav` command](../reference/command-line/common/repository-connect-webdav/). Read the [help docs](../reference/command-line/common/repository-connect-webdav/) for more information on the options available for this command.

## SFTP

Creating a SFTP or SSH `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

### Kopia GUI

Select the `SFTP Server` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `Host`, `User`, `Path`, and either `Password` or `Path to key file`. You can optionally enter `Path to known_hosts file`.

If the connection to SFTP server does not work, checking the option for `Launch external password-less SSH command` which will launch an external `ssh` process  that supports more connectivity options and may be needed for some hosts.

You will next need to enter the repository password that you want. This password can be whatever you want, it does not need to be the same as your SFTP password. In fact, it should not be the same! Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create sftp` command](../reference/command-line/common/repository-create-sftp/) to create a `repository`:

```shell
$ kopia repository create sftp \
        --path=... \
        --host=... \
        --username=... \
        --sftp-password=...
```

OR

```shell
$ kopia repository create sftp \
        --path=... \
        --host=... \
        --username=... \
        --keyfile=...
```

If the connection to SFTP server does not work, try adding `--external` which will launch an external `ssh` process that supports more connectivity options and may be needed for some hosts.

At a minimum, you will need to enter the path, host, username, and either password or path to key file. You may also need to include `--known-hosts`. There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-sftp/) for more information.

You will be asked to enter the repository password that you want. This password can be whatever you want, it does not need to be the same as your SFTP password. In fact, it should not be the same! Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect sftp` command](../reference/command-line/common/repository-connect-sftp/). Read the [help docs](../reference/command-line/common/repository-connect-sftp/) for more information on the options available for this command.

## Rclone

[Rclone](https://rclone.org/) is an open-source program that allows you to connect to various cloud storage platforms. Many of these platforms are already supported natively by Kopia (see above), but some are not. If you want to use Kopia to backup to cloud storage that Rclone supports but Kopia does not yet, then you can use Kopia's Rclone `repository` feature to do just that. The best part is that once you setup the Rclone `repository`, Kopia manages Rclone for you (including running Rclone when needed), so you do not need to do anything else after setup except make sure you [enable Rclone's self-update feature](https://rclone.org/commands/rclone_selfupdate/) so that it stays up-to-date.

> WARNING: Rclone support is experimental. In theory, all Rclone-supported storage providers should work with Kopia. However, in practice, only Dropbox, OneDrive, and Google Drive have been tested to work with Kopia through Rclone.

Before you can create an Rclone `repository` in Kopia, you first need to download/install Rclone and setup what is called an Rclone `remote` for the cloud storage you want to use. Do the following:

1. Download Rclone from [the Rclone website](https://rclone.org/); it is a single executable like Kopia, so you do not need to install it but do remember the path on your machine where you save the Rclone executable file because you will need to know it when setting up your `repository` in Kopia
2. Configure Rclone to setup a `remote` to the storage provider you want to use Kopia with; see [Rclone help docs](https://rclone.org/docs/) to understand how to do that

### Kopia GUI

Select the `Rclone Remote` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `Rclone Remote Path` and `Rclone Executable Path`. The `Remote Path` is `my-remote:/some/path`, where you should replace `my-remote` with the name of the Rclone `remote` you created earlier and replace `/some/path` with the directory on the cloud storage where you want Kopia to save your snapshots. The `Executable Path` is the location on your machine where you saved the Rclone executable that you downloaded earlier.

You will next need to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/) and arguments to Rclone, can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data! Remember, Kopia manages Rclone for you, so you do not need to do anything further with Rclone.

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create rclone` command](../reference/command-line/common/repository-create-rclone/) to create a `repository` (assuming `my-remote` is the name of the Rclone `remote` you created earlier and `/some/path` is the directory on the cloud storage where you want Kopia to save your snapshots):

```shell
$ kopia repository create rclone --rclone-exe=/path/to/rclone/executable --remote-path=my-remote:/some/path
```

There are also various other options (such as [actions](../advanced/actions/) and arguments to Rclone) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-rclone/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

Remember, Kopia manages Rclone for you, so you do not need to do anything further with Rclone once you have created the `repository`.

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect rclone` command](../reference/command-line/common/repository-connect-rclone/). Read the [help docs](../reference/command-line/common/repository-connect-rclone/) for more information on the options available for this command.

## Local or Network-attached Storage

Kopia allows you to save your snapshots on your local machine, network-attached, or any other readable directory that is attached to your local machine (such as USB device, SMB directory, SSHFS mount, etc.). All of these storages fall under the `filesystem` label.

Creating a filesystem `repository` is done differently depending on if you use Kopia GUI or Kopia CLI.

### Kopia GUI

Select the `Local Directory or NAS` option in the `Repository` tab in `KopiaUI`. Then, follow on-screen instructions.  You will need to enter `Directory Path`.

You will next need to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password! At this same password screen, you have the option to change the `Encryption` algorithm, `Hash` algorithm, `Splitter` algorithm, `Repository Format`, `Username`, and `Hostname`. Click the `Show Advanced Options` button to access these settings. If you do not understand what these settings are, do not change them because the default settings are the best settings.

> NOTE: Some settings, such as [actions](../advanced/actions/), can only be enabled when you create a new `repository` using command-line (see next section). However, once you create the `repository` via command-line, you can use the `repository` as normal in Kopia GUI: just connect to the `repository` as described above after you have created it in command-line.

Once you do all that, your repository should be created and you can start backing up your data!

### Kopia CLI

#### Creating a Repository

You must use the [`kopia repository create filesystem` command](../reference/command-line/common/repository-create-filesystem/) to create a `repository`:

```shell
$ kopia repository create filesystem --path=...
```

There are also various other options (such as [actions](../advanced/actions/)) you can change or enable -- see the [help docs](../reference/command-line/common/repository-create-filesystem/) for more information.

You will be asked to enter the repository password that you want. Remember, this [password is used to encrypt your data](../faqs/#how-do-i-enable-encryption), so make sure it is a secure password!

#### Connecting to Repository

After you have created the `repository`, you connect to it using the [`kopia repository connect filesystem` command](../reference/command-line/common/repository-connect-filesystem/). Read the [help docs](../reference/command-line/common/repository-connect-filesystem/) for more information on the options available for this command.
