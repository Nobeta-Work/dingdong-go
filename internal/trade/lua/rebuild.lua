if redis.call('HGET', KEYS[1], 'epoch') ~= ARGV[1]
    or redis.call('HGET', KEYS[1], 'builder') ~= ARGV[2]
    or redis.call('HGET', KEYS[1], 'ready') ~= '0' then return 0 end
for i=3,#ARGV do
    local ticket = cjson.decode(ARGV[i])
    local key = ticket.userId .. ':' .. ticket.requestId
    redis.call('HSET', KEYS[2], ticket.userId, key)
    redis.call('HSET', KEYS[3], key, ARGV[i])
end
return 1
