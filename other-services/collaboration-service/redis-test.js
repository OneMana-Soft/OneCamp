import { Redis } from 'ioredis';

const redis = new Redis({
    host: process.env.REDIS_HOST || 'redis',
    port: process.env.REDIS_PORT ? parseInt(process.env.REDIS_PORT) : 6379,
    password: process.env.REDIS_PASSWORD,
});

redis.on('connect', () => console.log('Connected to Redis'));
redis.on('error', (err) => console.error('Redis Error:', err));

try {
    const result = await redis.ping();
    console.log('Redis Ping Result:', result);
} catch (err) {
    console.error('Ping Failed:', err);
} finally {
    redis.disconnect();
}
