local epoch = redis.call('HGET', KEYS[1], 'epoch')
if not epoch or redis.call('HGET', KEYS[2], '__epoch__') ~= epoch
    or redis.call('HGET', KEYS[3], '__epoch__') ~= epoch then return -1 end
local key = ARGV[1] .. ':' .. ARGV[2]
local payload = redis.call('HGET', KEYS[3], key)
if ARGV[3] == 'ACK' then
    if payload then
        local ticket = cjson.decode(payload)
        ticket.state = 'DURABLE'
        redis.call('HSET', KEYS[3], key, cjson.encode(ticket))
    end
    redis.call('ZREM', KEYS[4], key)
    return 1
end
-- 只有数据库已落 FAILED 才能调用回补，且必须仍持有同一 requestId。
-- 不要求旧 epoch 相等：重建快照可能包含旧请求，持有者匹配可安全回补新代次。
if redis.call('HGET', KEYS[2], ARGV[1]) == key then
    redis.call('HDEL', KEYS[2], ARGV[1])
    local quota = tonumber(redis.call('HGET', KEYS[1], 'quota'))
    local remaining = tonumber(redis.call('HGET', KEYS[1], 'remaining'))
    redis.call('HSET', KEYS[1], 'remaining', math.min(quota, remaining + 1))
end
redis.call('HDEL', KEYS[3], key)
redis.call('ZREM', KEYS[4], key)
return 1
