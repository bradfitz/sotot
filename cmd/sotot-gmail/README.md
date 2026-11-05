# sotot-gmail

A sotot source for email needing attention. By default each Gmail thread
in your inbox with the label `todo` becomes a thing on the dashboard, with
its subject, the latest sender, and a link that opens it. Remove the label
or archive the thread and it disappears within a couple of minutes.

It reads Gmail as you, with the narrowest Gmail scope there is,
`gmail.metadata`: it can see labels and headers (subject, sender, date) but
**never message bodies**, and it can't change, send, or delete anything.

## One-time setup

Gmail can't be read with a service account for a personal `@gmail.com`
account, so this uses an OAuth client in your own Google Cloud project,
which you authorize once. About ten minutes of clicking.

*Status: written from Google's documentation (November 2026); not yet
walked through end to end.*

### 1. A Google Cloud project

Use a project of its own, so its consent screen is just for this. In the
[Google Cloud console](https://console.cloud.google.com/), open the project
picker at the top and click **New project**; name it e.g. `sotot`. Or:

    gcloud projects create sotot-PICK-A-SUFFIX --name=sotot

### 2. Enable the Gmail API

With the project selected: **APIs & Services > Library**, search for
**Gmail API**, open it, and click **Enable**. Or:

    gcloud services enable gmail.googleapis.com --project=sotot-PICK-A-SUFFIX

### 3. Configure the consent screen

Menu **> Google Auth platform**. It says "Google Auth platform not
configured yet"; click **Get started**:

1. **App Information**: App name `sotot`; User support email: you. **Next**.
2. **Audience**: **External** (Internal is only for Google Workspace
   organizations). **Next**.
3. **Contact Information**: your email. **Next**.
4. **Finish**: check the box agreeing to the Google API Services User Data
   Policy, **Continue**, then **Create**.

### 4. Publish it (important)

Go to **Google Auth platform > Audience** and, under Publishing status,
click **Publish app**, then confirm. The status becomes **In production**.

Don't skip this: while an External app is in **Testing**, Google expires
its refresh tokens after 7 days, and sotot-gmail would need logging in
again every week. Publishing doesn't make anything public; Google just
shows an "unverified app" warning when you authorize it, which is fine for
an app only you use.

### 5. Declare the scope

**Google Auth platform > Data Access > Add or remove scopes**. Filter for
`gmail.metadata`, check `.../auth/gmail.metadata` ("View your email message
metadata such as labels and headers, but not the email body"), click
**Update**, then **Save**.

### 6. Create the OAuth client

**Google Auth platform > Clients > Create client**:

- Application type: **Desktop app**
- Name: `sotot-gmail` (only shown in the console)

Click **Create**, then **Download JSON** in the dialog that appears. Do it
then: the client secret may not be shown again. Save the file somewhere
private, e.g. `~/keys/sotot-gmail-client.json`, mode 600.

(A Desktop app's client secret isn't really secret, per Google: the login
uses PKCE. The token file written next is the real secret.)

### 7. Authorize

    sotot-gmail login --client=$HOME/keys/sotot-gmail-client.json \
        --token=$HOME/keys/sotot-gmail-token.json

It prints a URL. Open it, choose your account, and on **Google hasn't
verified this app** click **Advanced**, then **Go to sotot (unsafe)**.
Allow viewing email message metadata, and continue.

If the browser is on the machine running `sotot-gmail login`, that's all.
Otherwise the last page fails to load (it redirects to `127.0.0.1` on the
browser's machine): copy that page's URL from the address bar and paste it
into the terminal.

It finishes by reading your Gmail profile, and writes the token file (mode
600). That file is all sotot-gmail needs from now on; keep it secret.

### 8. Make the label

In Gmail, create a label named `todo` (or pass `--labels=INBOX,whatever`).

## Running

    SOTOT_GMAIL_TOKEN_FILE=$HOME/keys/sotot-gmail-token.json \
    SOTOT_ADDR_FILE=$HOME/keys/sotot/gmail.addr \
        sotot-gmail

Check it first with `--dry-run`, which prints what it would publish
without connecting to sotot.

Flags:

- `--labels=INBOX,todo`: labels a thread must all have.
- `--interval=2m`: how often to check.
- `--account=N`: which signed-in Google account links open in
  (`mail.google.com/mail/u/N`).

## When access stops working

Google revokes the authorization if you change your Google password,
remove sotot's access at <https://myaccount.google.com/connections>, or it
goes unused for six months. sotot-gmail then shows "sotot can no longer
read Gmail: run sotot-gmail login" on the dashboard. Run step 7 again.
