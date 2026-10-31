# sampleapp

A tiny HTTP service used as a LocalPilot end-to-end fixture.

- `GET /users/{id}` returns a user.
- `POST /orders` creates an order.

Both endpoints require a bearer token issued by the auth service.
