local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
local members = redis.call('ZRANGEBYSCORE', KEYS[4], 1, now-5000, 'LIMIT', 0, 100)
local result = {}
for _,key in ipairs(members) do
    local payload = redis.call('HGET', KEYS[3], key)
    if not payload then return redis.error_reply('预扣索引缺少记录，需重建') end
    table.insert(result, payload)
end
return result
