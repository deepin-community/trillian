// Code generated by quota/redis/redistb/gen.go. DO NOT EDIT.
// source: update_token_bucket.lua

package redistb

import (
	"github.com/go-redis/redis"
)

// contents of the 'updateTokenBucket' Redis Lua script
const updateTokenBucketScriptContents = "--[[\n\nLICENSE\n===================\n\nCopyright 2017 Google LLC. All Rights Reserved.\n\nLicensed under the Apache License, Version 2.0 (the \"License\");\nyou may not use this file except in compliance with the License.\nYou may obtain a copy of the License at\n\n    http://www.apache.org/licenses/LICENSE-2.0\n\nUnless required by applicable law or agreed to in writing, software\ndistributed under the License is distributed on an \"AS IS\" BASIS,\nWITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.\nSee the License for the specific language governing permissions and\nlimitations under the License.\n\nTOKEN BUCKET\n===================\n\nScript to read and update a token bucket maintained in Redis. This is an\nimplementation of the token bucket algorithm which is a common fixture seen in\nrate limiting:\n\n    https://en.wikipedia.org/wiki/Token_bucket\n\nFor each key prefix, we maintain three values:\n\n    * `<prefix>.tokens`: Number of tokens in bucket at refresh time.\n\n    * `<prefix>.refreshed`: Time in epoch seconds when this prefix's bucket was\n      last updated.\n\n    * `<prefix>.refreshed_us`: The microsecond component of the last updated\n      time above. Stored separately because a Unix epoch with a microsecond\n      component brushes up uncomfortably close to integer boundaries.\n\nThe basic strategy is to, at update/read time, fill in all tokens\nthat would have accumulated since the last update, and then if\npossible deduct the number of requested tokens (or disallow the\nrequested action if there are not enough tokens).\n\nThe approach relies on the atomicity of EVAL in redis - only 1 command (EVAL or\notherwise) will be running concurrently per shard in the Redis cluster. Redis\nand Lua are very fast, so in practice this works out okay.\n\nA note on units: all times (instants) are measured in epoch seconds with a\nseparate microsecond component, durations in imicroseconds, and rates in\ntokens/second (e.g., a rate of 100 is 100 tokens/second).\n\nFor debugging, I'd recommend adding Redis log statements and then tailing your\nRedis log. Example:\n\n    redis.log(redis.LOG_WARNING, string.format(\"rate = %s\", rate))\n\n--]]\n\n--\n-- Constants\n--\n-- Lua doesn't actually have constants, so these are constants by convention\n-- only. Please don't modify them.\n--\n\nlocal MICROSECONDS_IN_SECOND = 1000000.0\n\n--\n-- Functions\n--\n\nlocal function subtract_time (base, base_us, leftover_time_us)\n    base = base - math.floor(leftover_time_us / MICROSECONDS_IN_SECOND)\n\n    leftover_time_us = leftover_time_us % MICROSECONDS_IN_SECOND\n\n    base_us = base_us - leftover_time_us\n    if base_us < 0 then\n        base = base - 1\n        base_us = MICROSECONDS_IN_SECOND + base_us\n    end\n\n    return base, base_us\nend\n\n--\n-- Keys and arguments\n--\n\nlocal key_tokens = KEYS[1]\n\n-- Unix time since the epoch in microseconds runs up uncomfortably close to\n-- integer boundaries, so we store time as two separate components: (1) seconds\n-- since epoch, and (2) microseconds with the current second.\nlocal key_refreshed = KEYS[2]\nlocal key_refreshed_us = KEYS[3]\n\nlocal rate = tonumber(ARGV[1])\nlocal capacity = tonumber(ARGV[2])\nlocal requested = tonumber(ARGV[3])\n\n-- Callers are allowed to inject the current time into the script, but note\n-- that outside of testing, this will always superseded by the time reported by\n-- the Redis instance so as to protect against clock drift on any particular\n-- local node.\nlocal now = tonumber(ARGV[4])\nlocal now_us = tonumber(ARGV[5])\n\n-- This is ugly, but all values passed in from Ruby get converted to strings\nlocal testing = ARGV[6] == \"true\"\n\n--\n-- Program body\n--\n\n-- See comment above.\nif testing then\n    if now_us >= MICROSECONDS_IN_SECOND then\n        return redis.error_reply(\"now_us must be smaller than 10^6 (microseconds in a second)\")\n    end\nelse\n    -- Scripts in Redis are pure functions by default which allows Redis to\n    -- replicate the entire script rather than the individual commands that it\n    -- contains. Because we're about to invoke `TIME` which produces a\n    -- non-deterministic result, we need to tell Redis to instead switch to\n    -- command-level replication for write operations. It will error if we\n    -- don't.\n    redis.replicate_commands()\n\n    local current_time = redis.call(\"TIME\")\n\n    -- Redis `TIME` comes back in two components: (1) seconds since epoch, and\n    -- (2) microseconds within the current second.\n    now = tonumber(current_time[1])\n    now_us = tonumber(current_time[2])\nend\n\nlocal filled_tokens = capacity\n\nlocal last_tokens = redis.call(\"GET\", key_tokens)\n\nlocal last_refreshed = redis.call(\"GET\", key_refreshed)\n\nlocal last_refreshed_us = redis.call(\"GET\", key_refreshed_us)\n\n-- Only bother performing rate calculations if we actually need to. i.e., The\n-- user has made a request recently enough to still be in the system.\nif last_tokens and last_refreshed then\n    last_tokens = tonumber(last_tokens)\n    last_refreshed = tonumber(last_refreshed)\n\n    -- Rejected a `now` that reads before our recorded `last_refreshed` time.\n    -- No reversed deltas are allowed.\n    if now < last_refreshed then\n        now = last_refreshed\n        now_us = last_refreshed_us\n    end\n\n    local delta = now - last_refreshed\n    local delta_us = delta * MICROSECONDS_IN_SECOND + (now_us - last_refreshed_us)\n\n    -- The time (in microseconds) that it takes to \"drip\" a single token. For\n    -- example, if our rate is 100 tokens per second, then one token is allowed\n    -- every 10^6 / 100 = 10,000 microseconds.\n    local single_token_time_us = math.floor(MICROSECONDS_IN_SECOND / rate)\n\n    local new_tokens = math.floor(delta_us / single_token_time_us)\n    filled_tokens = math.min(capacity, last_tokens + new_tokens)\n\n    -- For maximum fairness, modify the last refresh time by any leftover time\n    -- that didn't go towards adding a token.\n    --\n    -- However, only bother with this if the bucket hasn't been replenished to\n    -- full capacity. If it was, the user has had more replenishment time than\n    -- they can use anyway.\n    if filled_tokens ~= capacity then\n        local leftover_time_us = delta_us % single_token_time_us\n        now, now_us = subtract_time(now, now_us, leftover_time_us)\n    end\nend\n\nlocal allowed = filled_tokens >= requested\nlocal new_tokens = filled_tokens\nif allowed then\n    new_tokens = filled_tokens - requested\nend\n\n-- Set a TTL on the values we set in Redis that will expire them after the\n-- point in time they would have been fully replenished, which allows us to\n-- manage space more efficiently by removing keys that don't need to be in\n-- there.\n--\n-- Keys that are ~always in use because their owners make frequent requests\n-- will be updated by this script constantly (which sets new TTLs), and\n-- never expire.\nlocal fill_time = math.ceil(capacity / rate)\nlocal ttl = math.floor(fill_time * 2)\n\n-- Redis will reject a expiry of 0 to `SETEX`, so make sure TTL is always at\n-- least 1.\nttl = math.max(ttl, 1)\n\n-- In our tests we freeze time. Because we can't freeze Redis' notion of time\n-- and want to make sure that keys we set within test cases don't expire, we\n-- forego the standard TTL that we would have set for just a long one to make\n-- sure anything we set expires well after the test case will have finished.\nif testing then\n    ttl = 3600\nend\n\nredis.call(\"SETEX\", key_tokens, ttl, new_tokens)\nredis.call(\"SETEX\", key_refreshed, ttl, now)\nredis.call(\"SETEX\", key_refreshed_us, ttl, now_us)\n\nreturn { allowed, new_tokens, now, now_us }\n"

// Redis Script type for the 'updateTokenBucket' Redis lua script
var updateTokenBucketScript = redis.NewScript(updateTokenBucketScriptContents)
