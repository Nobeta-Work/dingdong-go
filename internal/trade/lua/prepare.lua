local epoch, owner = ARGV[1], ARGV[2]
local old = redis.call('HGET', KEYS[1], 'epoch')
-- 代次用十进制字符串比较，不依赖 Lua double 对 BIGINT 的精度。
if old and (#old > #epoch or (#old == #epoch and old > epoch)) then return -1 end
if old == epoch and redis.call('HGET', KEYS[1], 'ready') == '1' then return 2 end
local clock = redis.call('TIME')
local now = tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000)
if old == epoch and redis.call('HGET', KEYS[1], 'builder') ~= owner
    and tonumber(redis.call('HGET', KEYS[1], 'buildUntil') or '0') > now then return 0 end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4])
redis.call('HSET', KEYS[1], 'epoch', epoch, 'builder', owner, 'buildUntil', now+15000,
    'ready', '0', 'remaining', ARGV[3], 'quota', ARGV[4], 'status', ARGV[5],
    'version', ARGV[6], 'starts', ARGV[7], 'ends', ARGV[8])
redis.call('HSET', KEYS[2], '__epoch__', epoch)
redis.call('HSET', KEYS[3], '__epoch__', epoch)
redis.call('ZADD', KEYS[4], 0, '__epoch__')
return 1
