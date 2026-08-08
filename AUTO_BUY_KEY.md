# kiro-market integration documentation

You can paste this entire document into the AI ​​and let it write the client application for you.
All fields, error codes, idempotency rules, and signature algorithms are included.

Base URL: `https://api.91kiro.com`

---

## 1. Run in three minutes

```bash
BASE=https://api.91kiro.com
KEY=usr-xxxxxxxx # Issued once after registration; if lost, it can only be rotated.

# View my account and balance
curl -s "$BASE/api/my/profile" -H "X-API-Key: $KEY"

# Check inventory
curl -s "$BASE/api/my/stock" -H "X-API-Key: $KEY"

# Get 3 US region keys (idempotent: repeated calls to the same client_order_id will produce completely identical results)
# zone If not specified, the default zone is US; to specify the European zone, you must explicitly specify "eu".
ORDER=$(openssl rand -hex 16)
curl -s -X POST "$BASE/api/my/purchase" \
  -H "X-API-Key: $KEY" -H "Content-Type: application/json" \
  -d "{\"count\":3,\"zone\":\"us\",\"client_order_id\":\"$ORDER\"}"
```

---

## 2. Authentication

Choose one of the two methods:

| Method         | Usage                                               | Applicable                  |
| -------------- | --------------------------------------------------- | --------------------------- |
| API Token      | `X-API-Key: usr-…` or `Authorization: Bearer usr-…` | Scripts, Server Integration |
| Session cookie | Automatically carried by the browser after login    | Webpage interface           |

Scripts should always use API tokens. Write requests using cookies require an additional CSRF header, which the script does not need to handle.

The token appears in plaintext only once during registration; the database only stores the hash.

**Token replacement must be done on the webpage.** (`POST /api/my/api-key/rotate` only accepts session authentication; it calls back using the API token.)
(403 session_required). This is intentional: otherwise, whoever gets your leaked token could use it to exchange for a new one and use it against you.
Locked out—and your script is using that replaced token, with no way to recover it. The token's been compromised, so you're logging into the webpage.
Replace it, or contact the operator to replace it.

---

## 3. Concept

| Terminology    | Meaning                                                                                                                                                                                                                                        |
| -------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Master Account | An AWS account's AK/SK, the source of all keys                                                                                                                                                                                                 |
| Train Number   | A batch of train numbers and their keys generated from a single master number. If the master number is lost, the entire train is lost.                                                                                                         |
| Public Bus     | Buses that are activated to "release inventory" will have their output added to the public inventory and are **available for purchase across the entire site**. The parent account can belong to the platform or be contributed by a customer. |
| Private Car    | A car designated for "Personal Use" can only be claimed by the owner of the original account, and it's free. It won't be added to the public inventory.                                                                                        |
| Points         | Account Balance. 1:1 top-up, deducted when claiming a Key                                                                                                                                                                                      |
| Holding Limit  | This is the maximum number of active keys you can hold simultaneously. Once this limit is reached, you cannot purchase more. Your quota will automatically be freed up when your current account expires.                                      |
| Retrieve       | Replay the order using the historical `order_id`, retrieving the original batch of keys. No duplicate charges.                                                                                                                                 |

### 3.1 Regions and Endpoints

After obtaining the key, select the endpoint by clicking its `region`. **The hostname formats for the two regions are different**, so you can't deduce it by concatenating strings.

| zone | region         | endpoint                                        |
| ---- | -------------- | ----------------------------------------------- |
| `us` | `us-east-1`    | `https://codewhisperer.us-east-1.amazonaws.com` |
| `eu` | `eu-central-1` | `https://q.eu-central-1.amazonaws.com`          |

Only the US region has the host `codewhisperer.<region>`; the European region has `codewhisperer.eu-central-1`.
**It doesn't parse at all.** The same REST API is on `q.<region>.amazonaws.com`. Using a European region key to access the US region...
The endpoint will return a 403 error, which looks like "bought a useless account", but it's actually just a typo.

Keys are bound to regions and cannot be used across regions. An order will only come from one region (determined by the `zone` when the order is placed).

---

## 4. Billing Rules (Important)

**The unit price is determined by the production quantity of the complete vehicle and can be found in the tiered table.** The more vehicles produced, the cheaper the price. The unit price is frozen the moment the account is opened, and subsequent price adjustments will not affect existing vehicle orders.

Points are only deducted when you access the purchase interface to claim the reward. There is no way to deduct money without you making any action.

**Shared inventory, first come, first served.** Each public vehicle goes directly into the public inventory after it is produced, and is not reserved for anyone else.
If you see available stock in `GET /api/my/stock/rounds`, you can buy it. Whoever calls the API first gets it.

The price is determined by **each Key’s own region** (the price may differ between the US and European regions for the same car), and the actual payment will be given for each key in the response.

If the balance is insufficient, `insufficient_balance` will be returned directly, **and partial transactions will not be executed**. Similarly, if the holding limit is exceeded, `insufficient_balance` will be returned.
`purchase_cap_reached` (see §7) — Both of these result in the entire order failing; you won't get half of it.

**Free access only applies to vehicles designated for "private use".** If your primary account is set to "private use", claiming these keys yourself will not deduct points.
The response includes "free": true and the reason for free.

\*\*However, it's not free when your main account is set to "release inventory"—that batch of goods is from public inventory, available for purchase across the entire site, and you'll buy it just like everyone else.
The normal unit price is deducted. The reward for contributing to the parent account goes through a different process (points are returned based on output, settled at the moment of output), it's not "buy it for free".
Otherwise, the same batch of goods would be charged twice, even though it was the goods that the paying customer should have received.

### 4.1 Warranty

After receiving the item, there is a **warranty period** (default 10 minutes, configured by the operator; the `warranty_minutes` value in `GET /api/my/stock` is the current value).

If this vehicle is deemed unusable during the warranty period, the points you paid for this batch of keys will be **automatically and fully refunded**, no application is required, and a `warranty_refund` webhook will be pushed. Refunds will cease after the countdown ends—the lifespan of a Kiro key is inherently uncertain, and the warranty covers "it's gone as soon as you get it."

- The warranty period for each key is fixed at the moment of delivery, and subsequent configuration changes by the operator will not affect delivered orders;
- Points received for free (for personal use) are non-refundable and not covered by warranty (`warranty_until` is empty);
- Refunds are recorded as `warranty` in the transaction history; if the key comes from a car posted by someone else, the money will be deducted from the poster's earnings (their account will receive a `clawback`).
  **Revoked keys are not covered by warranty** (status is changed to `revoked`, see §5.2). Publicly distributing a key will result in dozens of users accessing the same key, causing the parent key to be flagged as abnormal activity by upstream providers and the entire system to fail prematurely—affecting everyone on the same system. In this case, we will revoke the key and **no points will be refunded**. Using it yourself or within your own team is fine.

---

## 5. Interface

All `/api/*` responses are JSON and include `Cache-Control: no-store`.

### 5.1 Account

#### `GET /api/my/profile`

```json
{
  "profile": {
    "id": "…",
    "username": "alice",
    "role": "user",
    "balance": 1400,
    "spent": 600,
    "earned": 0,
    "max_keys_held": 20,
    "hold_cap_effective": 10,
    "keys_held": 7,
    "api_key_prefix": "usr-1a2b3c4d",
    "webhook_private_url": "",
    "webhook_public_url": "https://…",
    "created_at": "2026-07-30T12:00:00Z",
    "last_login_at": "…"
  },
  "auth_mode": "api_key"
}
```

#### `PUT /api/my/settings`

```json
{ "max_keys_held": 20 }
```

`max_keys_held` ranges from 0 to 1000, and is the **holding limit**: the number of **active** keys under your name. Once this limit is reached, you cannot buy more.
0 = Unlimited. Your account limit will automatically be freed up if your account is suspended or deactivated; you don't need to do anything.

The operator also has a **global hard cap**, which is the stricter of the values ​​you set—so the profile contains three values ​​simultaneously:

| Field                | Meaning                                                                                             |
| -------------------- | --------------------------------------------------------------------------------------------------- |
| `max_keys_held`      | A value you set yourself                                                                            |
| `hold_cap_effective` | The upper limit of the **actually effective** value after stacking global hard caps (0 = unlimited) |
| `keys_held`          | The number of keys currently active under your name                                                 |

Before making a purchase, use `keys_held` and `hold_cap_effective` to check, which can save you from a potentially failed order.

#### `POST /api/my/password`

```json
{ "old_password": "…", "new_password": "…" }
```

Upon successful completion, **login status for all devices will be invalidated** (API tokens will not be affected).

#### `POST /api/my/api-key/rotate`

No request body. Returns a new token; the old token becomes invalid immediately.

**Session authentication is only accepted (called after web login).** Use the API token to call it back to `403 session_required` — see the reason below.
Section 2. Do not include this interface in the script.

### 5.2 Inventory and Train Numbers

#### `GET /api/my/stock`

```json
{
  "stock": {
    "public_available": 12,
    "my_private": 0,
    "my_keys": 27
  },
  "zones": [
    { "zone": "us", "region": "us-east-1", "available": 8, "unit_price": 25, "base_price": 40 },
    { "zone": "eu", "region": "eu-central-1", "available": 4, "unit_price": 10, "base_price": 10 }
  ],
  "max": 12,
  "min_per_order": 1,
  "max_per_order": 200,
  "warranty_minutes": 10
}
```

| Field                             | Meaning                                                                                                                                                                                                                                    |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `public_available`                | Total remaining public bus tickets available for purchase, first come, first served                                                                                                                                                        |
| `my_private`                      | The number of free coupons you can claim in your own car                                                                                                                                                                                   |
| `my_keys`                         | Total number of keys you have claimed                                                                                                                                                                                                      |
| `zones`                           | **Available quantity and unit price for each zone**. All zones are listed in a fixed order of `us`, `eu`. Zones without stock will have `available` set to 0. Specify the zone you want to purchase by sending it to the purchase request. |
| `max`                             | The maximum quantity that can be withdrawn at one time (= common surplus, capped at 200). **When polling for backup, check if it is > 0 before withdrawing.**                                                                              |
| `min_per_order` / `max_per_order` | Maximum and minimum quantity for a single pickup (1 / 200)                                                                                                                                                                                 |
| `warranty_minutes`                | Current warranty duration (minutes), 0 indicates not enabled                                                                                                                                                                               |

The unit price is independent for each region (example: US region `us` 25 points/each, Europe region `eu` 10 points/each).

`zones[].unit_price` is the **cheapest tier currently available in that zone, and has already been reduced to the current price based on the length of time the unit has been sold**. It can be shown to you directly.
User; `base_price` is the base price for the same price range (the two are equal when there is no price reduction). Use it if you want to display "Original price 40 → Current price 25".

⚠️ These two numbers are snapshots taken at the moment the data is retrieved. The price decreases by one level every few minutes based on the duration of each train's operation (§6).
Therefore, please refresh the displayed price with the polling cycle and avoid caching for too long. The actual discount is still based on the `total_credits` in the purchase response.
(When multiple vehicles are in the same area at the same time, the price may be mixed, see §5.3).

#### `GET /api/my/rounds`

Return only vehicles that are relevant to you: those driven by your main account, and those from which you purchased keys.

```json
{
  "rounds": [
    {
      "id": "…",
      "mother_id": "…",
      "owner_id": "…",
      "visibility": "public",
      "scope": "platform",
      "state": "live",
      "keys_total": 20,
      "unit_price": 30,
      "launched_at": "…",
      "died_at": "",
      "death_reason": "",
      "is_mine": false
    }
  ],
  "total": 1
}
```

Train status: `preparing` (Preparing) → `standby` (Standby) → `live` (Running) → `dying` (Dying) → `dead` (Dead)
There are also `failed` (failed to start the account) and `scrapped` (the original account was already dead before the start of the race).

#### `GET /api/my/keys`

List of keys that have been claimed. **Only the prefix is ​​provided, not the full text.** — For the complete text, please refer to §5.3.

Values ​​for `status`:

| Value     | Meaning                                                                                                                                 |
| --------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `sold`    | Normal, delivered to you                                                                                                                |
| `dead`    | Detection confirmed failure (this will also occur when the entire vehicle is deemed dead). Automatic refund within the warranty period. |
| `revoked` | Revoked, invalidated, and **no points refunded**. See the last item in §4.1 for the reason.                                             |

To determine if a key is still usable, look at this field, not `remaining`.

### 5.3 Receiving and Replenishing

#### `POST /api/my/purchase` (equivalent alias `POST /api/me/purchase`)

```json
{ "count": 5, "zone": "us", "client_order_id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9" }
```

`client_order_id` is a **32-bit hexadecimal** idempotent key, which can also be passed in the `Idempotency-Key` request header (providing both and if they don't match will result in a 400 error). If not passed, it will be generated by the server, but then you won't be able to replay it.

`count` ranges from 1 to 200.

**Zones** are strictly separated by zone; restocking across zones is prohibited.

| Pass Value      | Behavior                                                                                                                                                                                                  |
| --------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Not uploaded    | **By default, only numbers are retrieved from the US region**; if a number is out of stock in the US region, it will be returned as out of stock, and a number from the European region will not be used. |
| `"us"`          | Only the US region (us-east-1)                                                                                                                                                                            |
| `"eu"`          | Only take the European zone (eu-central-1)                                                                                                                                                                |
| Other values ​​ | Directly output **400 `bad_zone`**, will not silently process as US zone                                                                                                                                  |

To get a number from the European region, you \*\*must explicitly pass `zone: "eu"`; this is the only way to access the European region.

response:

```json
{
  "client_order_id": "0a1b2c3d4e5f60718293a4b5c6d7e8f9",
  "order_id": "…",
  "zone": "us",
  "purchased": 5,
  "unit_price": 30,
  "total_credits": 150,
  "remaining": 4500,
  "keys": [
    {
      "id": "…",
      "round_id": "…",
      "key": "ksk_…",
      "region": "us-east-1",
      "zone": "us",
      "free": false,
      "paid": 30,
      "warranty_until": "2026-08-01T12:34:56Z"
    }
  ],
  "free_count": 0,
  "warranty_until": "2026-08-01T12:34:56Z",
  "warranty_minutes": 10
}
```

**Please ensure the results are processed as `purchased`, not `count`.** Inventory is subject to concurrent competition; requesting 5 items and receiving 3 is a normal result, and fees will only be deducted based on the actual number of items sold.

**All reconciliations should be based on `total_credits`, not `unit_price × quantity`.** Multiple live vehicles can operate simultaneously in the same area, and the unit price is determined by **each vehicle's output**, so a single order may contain keys with different prices (delivery is based on the earliest received item, which may cross vehicle batches). In such mixed-price orders, `unit_price` only represents one batch's price, and multiplying it will result in a discrepancy between `total_credits` and the actual deduction.
`keys[].paid` is the authoritative value, and the sum of `paid` in each key is always equal to `total_credits`.

`keys` is an array of objects, each element containing a `key` (which is the product) and a `region` / `zone` (which determines which endpoint to hit, see §3.1). `paid` is the actual points deducted for each round (the amount refundable under warranty); `warranty_until` being empty indicates that there is no warranty for this round (this is the case for free delivery).

**Changed 2026-08-07: `account` / `password` / `issuer_url` will no longer be issued.**

> Those three items are the **web login credentials** for the sub-account, which are different from the API Key you need to use—you only need the `key` to call the API.
> The reason for removing it is security: if the same sub-account is used both by us from a fixed exit and by you logging into the webpage from your own IP address,
> From AWS's perspective, this means "the same credential is used on multiple IPs," which is the most typical characteristic of credential leakage.
> This will result in the **entire account being banned**, and all the keys you have will become invalid. Providing fewer of these three items is to ensure the accounts you buy last longer.
>
> At the same time, `endpoint` will no longer be issued: it is uniquely determined by `region`, which is a fixed lookup table (§3.1).
> There's no need to repeatedly upload each key.

Repeated calls to the same `client_order_id` will return results with **completely identical bytes**, and will not incur double charges.

#### `GET /api/my/orders/{order_id}/keys`

Retrieve the order by order number and return the original delivery result. This is the corresponding interface for webhook notifications—the notification doesn't contain the key; you exchange it for `order_id`.

#### `GET /api/my/orders` (equivalent alias `GET /api/my/purchase-orders`)

Historical order list, supports `?limit=&offset=` (limit capped at 200). Each entry contains `id` / `client_order_id` /
`count` / `unit_price` / `charged` / `free_count` / `created_at` —— 对账用 `charged`
(That's the actual deduction value. In the mixed price list, `unit_price` cannot be multiplied by the quantity, see above.)

### 5.4 points

#### `POST /api/my/redeem`

```json
{ "code": "XXXXXXXXXX" }
```

Returns `{"quota": 500, "balance": 1900}`. Case sensitivity and hyphens in the redemption code will be ignored.

#### `GET /api/my/ledger`

Integral flow. `reason` values:

| Value      | Meaning                                                                                                                                                                     |
| ---------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `recharge` | Redemption code recharge                                                                                                                                                    |
| Purchase   | Fee deducted when you claim the Key (the only time this fee will be deducted)                                                                                               |
| `income`   | If someone buys the Key you gave to the automated platform: you will receive a refund equal to `actual payment × (100 - platform service fee%)`.                            |
| `warranty` | Warranty refund: If a train service is deemed invalid within the warranty period, deducted points will be refunded.                                                         |
| `clawback` | Warranty refund reversal: If the key you provided is refunded, the **original amount allocated to you** will be reversed from your account (not the entire purchase price). |
| `adjust`   | Manual adjustment by the operator                                                                                                                                           |
| `commit`   | Legacy: Earlier versions automatically deducted fares upon departure; this no longer generates new transaction records.                                                     |

### 5.5 My AWS Account (Main Contribution Account)

#### `POST /api/my/mothers`

```json
{
  "label": "Main number",
  "access_key": "AKIA...",
  "secret_key": "...",
  "gen_mode": "group",
  "tier": 20,
  "overage_pref": false,
  "region": "us-east-1",
  "note": "",
  "pool": "public"
}
```

The system will immediately verify the credentials using STS during data entry; if the verification fails, the application will be rejected.

**AK/SK encryption is stored in the database. Afterwards, any interface will only echo the last four digits of the AK; the SK will never be echoed.**

The `tier` can only be `20 / 40 / 100 / 200` (USD tiers).

`pool` determines whether this number will be used by an **automatic vehicle**. If not passed, it will be handled as `public`.

| Value     | Meaning                                                                                                                                                                                                                                                                                                                                                               |
| --------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `public`  | It will join the queue of automated vehicles. When it's its turn, it will create a sub-account and open a subscription using this AWS account (**which will generate an AWS bill**). The output will be added to the public inventory and sold on a first-come, first-served basis. After someone else buys it, you will receive points according to the rules below. |
| `private` | It only needs to be manually activated by the operations team; the automated vehicle will not retrieve it.                                                                                                                                                                                                                                                            |

Pool and ownership are two different things: the account always belongs to you (return points, pricing standards, and deletion permissions all follow ownership).
`pool` only determines whether the autonomous vehicle should pick it up.

**Price Reduction Based on Survival Time:** Operators can allocate a price reduction of M points every N minutes, capped at F points, to a specific area. The price will be reduced from the **departure time**.
Starting from the latest price, the older the product, the cheaper it is:

```
Current price = max(benchmark price − ⌊living minutes / N⌋ × M, F)
```

To display the price, read the current price field directly, not `unit_price`:
The `zones[].unit_price` value in `GET /api/my/stock` is already the current price after the price drop (`base_price` is the base price).
Each line in the `GET /api/my/stock/rounds` request includes an additional `current_price`. Both are used by the server for billing.
The result calculated using the same formula is valid only at the moment the number is retrieved.

- `GET /api/my/stock/rounds` still includes `decay_minutes` / `decay_amount` / `price_floor` on each line.
  Similar to `launched_at`, you can use them with the formula above if you want to recalculate locally every minute (instead of relying on polling refresh).
  If all three parameters are 0, it means that this group has not implemented a price reduction.
- ⚠️ `unit_price` in `stock/rounds` and `GET /api/my/rounds` and `GET /api/my/orders`
  The `unit_price` values ​​are the **base price**. Even after a price reduction is enabled, it doesn't necessarily mean the amount you'll be charged. — Use it to display...
  This can result in a situation where "the page displays 40, but 25 is actually deducted." The actual payment is calculated based on the lifespan of the key at the moment of order placement and written into the `paid` field of each key.
- Price reductions only apply to **goods that haven't been sold yet**; the paid portion for delivered goods remains unchanged.

**Sales commission**: When someone buys the key you gave to the automated vehicle, you receive [a commission].

```
The refund to you = your actual payment for this round × (100 − contribute_service_fee_pct) / 100 (rounded down)
```

- Service fees are set by operations; `0` = full refund of sales amount, `100` = full retention by the platform (default).
- **Pricing is not differentiated by ownership**: The platform's own master accounts and master accounts submitted by customers are sold at the same price (one price for the US region and one for the European region).
  Ownership only determines who receives a share of the profits after the sale.
- **Calculated based on the actual cost per key**, not the average price or the price per trip: Prices differ between the US and Europe, and a single order may include items from both regions.
  The goods will not be returned incorrectly.
- Rate changes are not retroactive: the amount returned for each item is fixed at the moment of delivery, and warranty refunds are also reversed based on the original amount.
- You buy the number yourself and hand it over to the automated vehicle **no profit sharing** (otherwise, with a service fee of 0, it's like getting it for free, and you'll know when it arrives before anyone else).
  (It's possible to clear the entire vehicle before paying customers do).
  **Exception: Package reservations will still be distributed as usual**, even if these packages happen to come from your own submitted master accounts. The reservation quantity and price...
  The purchases are all fixed in the agreement by the operations team and subject to the reserved upper limit. Since it's not a purchase you initiated yourself, the concern above is unfounded.
  Without this exception, a reversed outcome would occur: someone else buys the same batch of goods from you, making a profit, while you retain your agreed-upon share.
  Instead, the full amount will be deducted from the agreed price. For warranty refunds on such orders, the commission will be reversed along with the refund, and the net amount will still only be the service fee portion.
- See §5.4 for transaction types: incoming transactions are "income", and warranty refund reversals are "clawback".

The other two fields in the parent view are used to view the queuing progress:

| Field            | Meaning                                                                                                                                                                      |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `queue_position` | The position in the queue of the pool (starting from 1). **0 indicates not in the queue** — the parent account is disabled, lacks Kiro privileges, or has unfinished trains. |
| `queue_total`    | The total length of the queues in this pool                                                                                                                                  |

Only one bus runs at a time, and the next bus will only depart after the previous one has finished running, so the ranking does not equal the time.

#### `POST /api/my/mothers/{id}/pool`

```json
{ "pool": "public" }
```

Modify the departure pool for this mother number. If there are unfinished trains on the mother number, return `409 mother_busy` (the train number does not have a designated pool).
Switching pools halfway through the process leaves the question of "which pool should this batch of output belong to" unanswered. Wait until it finishes running before making changes.

其他：`GET /api/my/mothers`、`PUT /api/my/mothers/{id}`、`POST /api/my/mothers/{id}/status`、`POST /api/my/mothers/{id}/verify`、`POST /api/my/mothers/{id}/quota`、`DELETE /api/my/mothers/{id}`。

Deleting a mother number is not allowed if there are still unfinished train services; please disable it first.

### 5.6 Webhook

#### `GET /api/my/webhook` / `PUT /api/my/webhook`

```json
{
  "private_url": "https://example.com/kiro/my-webhook",
  "public_url": "https://example.com/kiro/webhook"
}
```

| Channel                 | When is it triggered?                     | Description                                                                                        |
| ----------------------- | ----------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Your own private URL    | Your own primary account has been created | Notification will only be sent to you. Leaving this blank will redirect to the public bus address. |
| Public bus `public_url` | Platform public pool replenishment        | Requires public network access; signature verification recommended.                                |

The address must be http/https, **and cannot point to intranet, loopback, or cloud metadata addresses**, nor can it contain account passwords in the URL.

其他：`POST /api/my/webhook/test`（`{"channel":"private"}` 或 `"public"`）、`POST /api/my/webhook/rotate`、`GET /api/my/webhook/deliveries`。

---

## 6. Webhook Payload and Signature

### Load

**Only metadata is included, not the key.** The replenishment event contains neither the key nor the order number—what you need to do is retrieve its `zone` and...
`purchase_order_id` is used to retrieve the order from the previous purchase. If the order has already been retrieved but you want to retrieve it again, you're using the order returned from that previous purchase.
`order_id` calls the pull interface in §5.3.

`new_keys_available` (Restocking, **you mainly rely on this**):

```json
{
  "event": "new_keys_available",
  "event_id": "A unique ID to be reused",
  "visibility": "public",
  Message: "20 new keys in the US region are ready for pickup".
  "new_keys": 20,
  "zone": "us",
  "purchase_order_id": "32-digit hexadecimal value, recommended to use directly as client_order_id",
  "pool_id": "The parent ID that produced this batch of goods, used for deduplication by parent ID",
  "timestamp": 1785000000
}
```

⚠️ **`purchase_order_id` is not an order number. Do not use it to call the API (it will result in a 404 error - there is no order at that time).
It's an **idempotent key\*\* that we pre-generated for this batch of goods: you pass it as the `client_order_id` to the purchaser.
When a webhook is re-delivered using the same value, the transaction will only be completed once, making it naturally idempotent.

This event **does not include** `order_id` / `round_id` / `unit_price` — to see the price, first check `GET /api/my/stock`.

`reserved_keys_delivered` (Price allocation has been delivered; **you will only receive it if you have signed a price allocation agreement**):

```json
{
  "event": "reserved_keys_delivered",
  "event_id": "A unique ID to be reused",
  "visibility": "public",
  Message: "3 Keys (22 points/key) have been delivered to the US region at the agreed price. Pick up using order_id; no further order is needed."
  "order_id": "The actual order number, used to retrieve the key from the API body",
  "round_id": "…", "mother_id": "…",
  "zone": "us", "region": "us-east-1", "new_keys": 3, "unit_price": 22,
  "timestamp": 1785000000
}
```

⚠️ **This is the exact opposite of how `new_keys_available` is handled**, don't confuse them:

|                       | `new_keys_available`                                  | `reserved_keys_delivered`                                                      |
| --------------------- | ----------------------------------------------------- | ------------------------------------------------------------------------------ |
| Meaning               | In stock, go buy it!                                  | Already bought it, it's yours!                                                 |
| Money                 | Not yet deducted                                      | Deducted in full according to the agreed price                                 |
| What to do            | Use `purchase_order_id` to call the purchase function | Use `order_id` to call the pull interface in §5.3 to retrieve the main content |
| Adjust purchase again | Normal transaction                                    | **We will buy another batch at the public price**, do not do this              |

The package allocation is ordered on your behalf by the server, so you don't make any requests or responses that can retrieve the key content.
The `GET /api/my/keys` command only provides a prefix—**the `order_id` in this notification is the only entry point to retrieve the main content.**
The consequence of omitting this step is that the money will be deducted and the account will be registered in your name, but you will never be able to access your program.

`all_keys_dead` (all keys for this vehicle are dead): includes `round_id` and `dead` (number of dead keys); the key for your own vehicle also includes `mother_id`.

`warranty_refund` (warranty refund has been credited): includes `round_id`, `refunded_quota`, `refunded_keys`, and `reason`.

Event type:

| `event`                   | Meaning                                                                                                                                                                                                                                                         |
| ------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `new_keys_available`      | New inventory is available for purchase. Includes `zone` (replenishment area) and `purchase_order_id` (**used as the idempotent key for pickup**, not the order number).                                                                                        |
| `reserved_keys_delivered` | The reserved key quantity has been delivered at the agreed price. **Money has been deducted, and the key is yours.** Includes `order_id` (use it to retrieve the main content), `zone`, `region`, `new_keys`, and `unit_price`. **Do not call purchase again.** |
| `all_keys_dead`           | All keys for this vehicle have expired. It's time to restart the vehicle. The key count is `dead` (number of expired keys).                                                                                                                                     |
| `warranty_refund`         | The train trip expired during the warranty period, and the points have been refunded. Includes `refunded_quota` and `refunded_keys`.                                                                                                                            |
| `webhook_test`            | The probe event when you click "Test"                                                                                                                                                                                                                           |

`zone` and `region` appear in two types of events: replenishment and reserved delivery (`new_keys_available`, `reserved_keys_delivered`). `zone` can be directly used as the `zone` parameter when picking up goods; `region` is the complete AWS region identifier (`us-east-1` / `eu-central-1`), which is used by customers for fine-grained routing.

### Request Headers

| Header                  | Description                      |
| ----------------------- | -------------------------------- |
| `X-KM-Event`            | Event Name                       |
| `X-KM-Event-Id`         | Event ID, used for deduplication |
| `X-KM-Timestamp`        | Unix seconds                     |
| `X-KM-Delivery-Attempt` | Number of attempts (maximum 3)   |
| `X-KM-Signature`        | `sha256=<hex>`                   |

### Signature Verification

The signature content is `timestamp + "." + original request body`:

```python
import hmac, hashlib

def verify(secret: str, timestamp: str, signature: str, body: bytes) -> bool:
    mac = hmac.new(secret.encode(), f"{timestamp}.".encode() + body, hashlib.sha256)
    return hmac.compare_digest("sha256=" + mac.hexdigest(), signature)
```

Please use the **raw bytes** for verification; do not parse and then re-serialize. It is also recommended to reject requests with a `timestamp` that is more than 5 minutes off.

### Retry

For non-2xx events, a total of 3 retries will be made, with increasing intervals and jitter. Multiple attempts for the same event will carry the same `event_id`; please use it to remove duplicates.

---

## 7. Error Codes

Uniform shape (`error` is an alias for `message`, it can be read by any field):

```json
{
  "code": "no_stock",
  "message": "No deliverable stock available, please try again later",
  "error": "No deliverable stock available, please try again later"
}
```

Prioritize judging `code` (a stability indicator), not `text` - `message`/`error` will be corrected.

| HTTP               | `code`                                                       | Handling suggestions                                                                                                                                                                                                                                     |
| ------------------ | ------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 400                | `bad_json`                                                   | The request body is not valid JSON or contains unknown fields.                                                                                                                                                                                           |
| 400                | `bad_order_id`                                               | Idempotent key is not a 32-bit hexadecimal value                                                                                                                                                                                                         |
| 400                | `bad_count`                                                  | Quantity exceeds 1–200                                                                                                                                                                                                                                   |
| 400                | `bad_zone`                                                   | `zone` is not `us` / `eu`. To access the European zone, you must explicitly pass `zone:"eu"`.                                                                                                                                                            |
| 400                | `idempotency_conflict`                                       | The idempotency key in the request body does not match the idempotency key in the request header.                                                                                                                                                        |
| 401                | `unauthenticated` / `invalid_api_key`                        | Check token                                                                                                                                                                                                                                              |
| 402                | `insufficient_balance`                                       | Insufficient balance, please top up first                                                                                                                                                                                                                |
| 403                | `disabled`                                                   | Account is disabled. Please contact the operator.                                                                                                                                                                                                        |
| 403                | `csrf_failed`                                                | CSRF header missing when calling the API using cookies; please use `X-API-Key` instead in the script.                                                                                                                                                    |
| 403                | `session_required`                                           | This operation can only be performed after logging in via the web page (currently, the only option is to change the API token). **Retrying with the token is useless.**                                                                                  |
| 404                | Not Found                                                    | The order/resource does not exist or does not belong to you.                                                                                                                                                                                             |
| 404                | `redeem_invalid`                                             | Redemption code does not exist/used/expired/deactivated (no distinction, to prevent enumeration)                                                                                                                                                         |
| 413                | `body_too_large`                                             | Request body exceeds 1 MiB                                                                                                                                                                                                                               |
| 409                | `no_stock`                                                   | No stock available, please try again later                                                                                                                                                                                                               |
| 409                | `purchase_cap_reached`                                       | Purchase cap reached, **retry is useless**: Increase `max_keys_held` or wait for your account to expire.                                                                                                                                                 |
| 409                | `retry_same_order`                                           | Inventory was claimed concurrently. **Retry with the same `client_order_id`.**                                                                                                                                                                           |
| 429                | `rate_limited`                                               | See the `Retry-After` header. Login/registration has an additional rate limit per IP address at the entry point (normal calls won't encounter this, but it will be blocked during concurrent floods); `POST /api/my/webhook/test` is limited by account. |
| 502                | `verify_failed` / `quota_failed`                             | The AWS hop failed, you can try again.                                                                                                                                                                                                                   |
| 500 Internal Error | Server-side issue, please try again or contact the operator. |

---

## 8. Recommended Client Flow

```
start up
 └─ GET /api/my/profile Confirm token availability and check balance
 └─ PUT /api/my/settings Sets the holding limit (optional, 0 = unlimited)
 └─ PUT /api/my/webhook with callback configured, POST …/test to verify once.

normal
 └─ Received new_keys_available
      └─ POST /api/my/purchase {count, client_order_id, zone} Both zone and client_order_id are from the event (the latter is taken from purchase_order_id).
      └─ Already claimed, just need to retrieve the key again → GET /api/my/orders/{order_id}/keys
 └─ Received all_keys_dead
      Mark this batch as invalid, and wait for the next new_keys_available entry.
 └─ Received warranty refund
      The money for the batch of goods that died during the warranty period has been refunded. You can reconcile the accounts using the refund_quota; no application is required.

Backup (when the webhook fails to deliver)
 └─ GET /api/my/stock/rounds every 60 seconds, and buy when you see remaining > 0.
```

Request failures are handled according to the table in §7; `retry_same_order` must reuse the same idempotent key.
Do not poll every second – there is a rate limit per account.

---

## 9. Remaining Credit Limit for Key

**Note the distinction between the two "integrals":**

| Concept                 | Meaning                                                             | Where to look                     |
| ----------------------- | ------------------------------------------------------------------- | --------------------------------- |
| **Platform Points**     | Your balance on this platform will be deducted when you claim a Key | `GET /api/my/profile`'s `balance` |
| **Key Remaining Quota** | How many more times can `ksk_` be called on the Kiro side?          | Interfaces in this section        |

The credit limit figure is a **snapshot**, not a real-time value. Checking Kiro every time it's displayed will immediately trigger the data throttling limit, so you need to actively synchronize it.

### `GET /api/my/usage`

Sum the remaining credit limit for all available keys under your name.

```json
{
  "usage": {
    "remaining": 4820,
    "total": 6000,
    "synced": 12,
    "keys": 15
  }
}
```

If `synced` is less than `keys`, it means there are still keys that were never successfully synchronized—those are not counted in `remaining`, and it doesn't mean "no credit limit".

### `POST /api/my/keys/{id}/usage`

Synchronizing a single key. The response is always 200:

```json
{
  "usage": {
    "key_id": "…",
    "used": 180,
    "max": 500,
    "remaining": 320,
    "subscription": "Kiro Pro",
    "reset_days": 12,
    "checked_at": "2026-07-31T01:20:00Z",
    "error": ""
  }
}
```

A non-empty `error` value indicates that the synchronization failed (usually due to Kiro rate limiting). **The numbers from the last successful synchronization are retained** and not cleared—so `used` / `max` still have the old valid values ​​when the synchronization fails.

### `POST /api/my/keys/usage/refresh`

Batch sync all available keys (expired keys will be skipped). The server has a concurrency limit, so Kiro will not be flagged as a 429 error.

```json
{
  "usages": [
    /* Same as above, one per entry */
  ],
  "total": 15,
  "failed": 2
}
```

Partial failures are normal. When `failed > 0`, please check the `error` entries one by one, and do not treat the entire entry as an API error.

### Suggested Usage

- Synchronize once after delivery, and then synchronize hourly thereafter. Do not synchronize before each request.
- If `remaining` drops below 10% of `max`, it's time to prepare to change the key.
- Synchronization failure does not affect the availability of the key itself; conversely, the presence of many `remaining` entries does not mean the key has not been blocked—its viability depends on the `status` of `GET /api/my/keys`.
