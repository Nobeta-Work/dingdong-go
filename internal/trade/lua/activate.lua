if redis.call('HGET', KEYS[1], 'epoch') ~= ARGV[1]
    or redis.call('HGET', KEYS[1], 'builder') ~= ARGV[2] then return 0 end
redis.call('HSET', KEYS[1], 'ready', '1')
return 1
