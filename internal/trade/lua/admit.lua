local epoch = redis.call('HGET', KEYS[1], 'epoch')
if not epoch or redis.call('HGET', KEYS[1], 'ready') ~= '1'
    or redis.call('HGET', KEYS[2], '__epoch__') ~= epoch
    or redis.call('HGET', KEYS[3], '__epoch__') ~= epoch
    or not redis.call('ZSCORE', KEYS[4], '__epoch__') then return {'REBUILD', ''} end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
if redis.call('HGET', KEYS[1], 'status') ~= 'PUBLISHED'
    or now < tonumber(redis.call('HGET', KEYS[1], 'starts'))
    or now >= tonumber(redis.call('HGET', KEYS[1], 'ends')) then return {'CLOSED', ''} end
local ticket = cjson.decode(ARGV[1])
local key = ticket.userId .. ':' .. ticket.requestId
local old = redis.call('HGET', KEYS[3], key)
if old then
    if cjson.decode(old).parameterHash ~= ticket.parameterHash then return {'CONFLICT', ''} end
    return {'OK', old}
end
if redis.call('HEXISTS', KEYS[2], ticket.userId) == 1 then return {'HELD', ''} end
if tonumber(redis.call('HGET', KEYS[1], 'remaining')) <= 0 then return {'QUOTA', ''} end
ticket.epoch = epoch
ticket.version = redis.call('HGET', KEYS[1], 'version')
ticket.createdMs = now
ticket.state = 'PREHELD'
local payload = cjson.encode(ticket)
redis.call('HINCRBY', KEYS[1], 'remaining', -1)
redis.call('HSET', KEYS[2], ticket.userId, key)
redis.call('HSET', KEYS[3], key, payload)
-- 不设置预扣 TTL；索引由可靠落库后的确认/回补显式移除。
redis.call('ZADD', KEYS[4], now, key)
return {'OK', payload}
